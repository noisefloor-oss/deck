package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Run the real launcher and trusted parser with a fake container/runtime and
// GitHub transport. The model's old unbound approval must never reach POST.
func TestReviewSnapshotPostingGate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq required")
	}
	binary := filepath.Join(t.TempDir(), "bump")
	build := exec.Command("go", "build", "-o", binary, "../cmd/noisedeck-harness-bump")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build parser: %v %s", err, out)
	}
	head, base := strings.Repeat("1", 40), strings.Repeat("2", 40)
	for _, mode := range []string{"valid", "unbound", "wrong-head", "duplicate-base", "stale-head", "stale-base", "missing-main", "api-error", "changed-diff"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for _, sub := range []string{"bin", "candidate", "lib"} {
				if err := os.Mkdir(filepath.Join(dir, sub), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for _, lib := range []string{"review.sh", "utils.sh", "token.sh", "jwt.sh"} {
				write("lib/"+lib, "# selected tool source\n")
			}
			write("lib/must-not-mount", "synthetic excluded file\n")
			write("diff", "net doc diff\n")
			write("evidence", `{"repair_diff":"-func  Fix()\n+func Fix()","current_check":"success"}`)
			write("bin/tool", "#!/bin/sh\nprintf 'dummy-app-token\\n'\n")
			write("bin/docker", `#!/bin/bash
set -eu
case "$1" in
 ps) exit 0 ;;
 run) shift ;;
 *) exit 90 ;;
esac
work=
library=
while [ "$#" -gt 0 ]; do
 case "$1" in
  --volume) case "$2" in *:/work:Z) work="${2%:/work:Z}" ;; *:/opt/review-lib:ro,Z) library="${2%:/opt/review-lib:ro,Z}" ;; esac; shift 2 ;;
  *) shift ;;
 esac
done
case "$library" in "$FIXTURE"/tmp.*/review-lib) ;; *) exit 91 ;; esac
[ -d "$library" ]
[ "$(find "$library" -type f | wc -l)" -eq 4 ]
[ ! -e "$library/must-not-mount" ]
for lib in review.sh utils.sh token.sh jwt.sh; do cmp "$library/$lib" "$FIXTURE/lib/$lib"; done
cat >"$FIXTURE/input"
grep -q "Reviewed head: $JAIL_HEAD" "$FIXTURE/input"
grep -q "Base: $JAIL_BASE" "$FIXTURE/input"
grep -q 'func  Fix()' "$FIXTURE/input"
printf 'APPROVE\n✅ VERDICT: APPROVE\n' >"$work/.noise-verdict"
if [ "$MODE" = unbound ]; then exit 0; fi
boundhead="$JAIL_HEAD"
if [ "$MODE" = wrong-head ]; then boundhead="$JAIL_BASE"; fi
printf 'Reviewed head: %s\nBase: %s\n' "$boundhead" "$JAIL_BASE" >>"$work/.noise-verdict"
if [ "$MODE" = duplicate-base ]; then printf 'Base: %s\n' "$JAIL_BASE" >>"$work/.noise-verdict"; fi
`)
			write("bin/gh", `#!/bin/bash
set -eu
if [ "$2" = --method ]; then
 test "$3" = POST
 cp "${@: -1}" "$FIXTURE/posted"
 jq --arg head "$JAIL_HEAD" '. + {commit_id:$head}' "$FIXTURE/posted"
else
 test "$2" = graphql
 [ "$MODE" != api-error ] || exit 1
 if [ "$MODE" = missing-main ]; then printf '{"data":{"repository":{"ref":null}}}\n'; exit 0; fi
 head="$JAIL_HEAD"; base="$JAIL_BASE"
 [ "$MODE" != stale-head ] || head="$base"
 [ "$MODE" != stale-base ] || base="$head"
 jq -n --arg head "$head" --arg base "$base" '{data:{repository:{ref:{target:{oid:$base}},pullRequest:{id:"PR7",headRefOid:$head,baseRefOid:"stale-recorded-base",mergeQueueEntry:null}}}}'
fi
`)
			cmd := exec.Command("bash", "../scripts/harness-bump/review-jail.sh")
			prompt := "tool-owned instructions\n## Diff\n\n```diff\nnet doc diff\n```\n"
			if mode == "changed-diff" {
				prompt = strings.ReplaceAll(prompt, "net doc diff", "raced diff")
			}
			cmd.Stdin = strings.NewReader(prompt)
			cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":" + os.Getenv("PATH"), "LD_LIBRARY_PATH=" + os.Getenv("LD_LIBRARY_PATH"), "TMPDIR=" + dir, "FIXTURE=" + dir, "MODE=" + mode,
				"JAIL_HEAD=" + head, "JAIL_BASE=" + base, "JAIL_REPO=acme/deck", "JAIL_PR=7", "JAIL_WORK=" + filepath.Join(dir, "candidate"), "JAIL_DIFF=" + filepath.Join(dir, "diff"), "JAIL_EVIDENCE=" + filepath.Join(dir, "evidence"), "JAIL_BUMP=" + binary, "JAIL_TOOL=" + filepath.Join(dir, "bin/tool")}
			for _, key := range []string{"JAIL_IMAGE", "JAIL_NODE", "JAIL_CODEX", "JAIL_GO", "JAIL_GOMOD", "JAIL_SOCKET", "JAIL_BROKER", "JAIL_MODEL"} {
				cmd.Env = append(cmd.Env, key+"=unused")
			}
			out, err := cmd.CombinedOutput()
			_, posted := os.Stat(filepath.Join(dir, "posted"))
			if mode == "valid" {
				if err != nil || posted != nil {
					t.Fatalf("valid snapshot failed: %v %s", err, out)
				}
			} else if err == nil || !os.IsNotExist(posted) {
				t.Fatalf("invalid snapshot reached posting: %v %s", err, out)
			}
		})
	}
}
