package sealedstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The measured layout of the development container: overlay root, btrfs
// bind mounts on an anonymous device the container cannot classify, a
// loop-backed ext4 /tmp, and a tmpfs /run/noisedeck.
const measuredMountInfo = `1429 1364 0:90 / / ro,relatime - overlay overlay rw,lowerdir=1/fs,upperdir=/u,workdir=/w
1430 1429 0:119 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw
1433 1429 0:122 / /sys ro,nosuid,nodev,noexec,relatime - sysfs sysfs ro,seclabel
1437 1429 0:35 /root/var/lib/docker/volumes/deck_deck-data/_data /data rw,relatime master:1 - btrfs /dev/nvme0n1p4 rw,seclabel,subvolid=256,subvol=/root
1438 1429 7:0 / /tmp rw,nosuid,nodev,noatime - ext4 /dev/loop0 rw,seclabel
1439 1429 0:35 /root/srv/noisefloor/apps/data/deck/projects /workspace rw,relatime - btrfs /dev/nvme0n1p4 rw,seclabel,subvolid=256,subvol=/root
1441 1429 0:35 /root/var/lib/docker/volumes/deck_deck-home/_data /home/agent rw,relatime master:1 - btrfs /dev/nvme0n1p4 rw,seclabel,subvolid=256,subvol=/root
1440 1429 0:124 / /run/noisedeck rw,nosuid,nodev,noexec,relatime - tmpfs tmpfs rw,size=16384k,mode=700,uid=1003,gid=1003
`

// A shared-host layout as the runbook composes it: every store on the
// partner's gocryptfs mount, /tmp included, owned by uid 1026.
const gocryptfsMountInfo = `1 0 0:90 / / ro,relatime - overlay overlay rw
10 1 0:200 /data /data rw,nosuid,nodev,relatime - fuse.gocryptfs /dev/fuse rw,user_id=1026,group_id=100,default_permissions,max_read=131072
11 1 0:200 /home /home/agent rw,nosuid,nodev,relatime - fuse.gocryptfs /dev/fuse rw,user_id=1026,group_id=100,default_permissions,max_read=131072
12 1 0:200 /projects /workspace rw,nosuid,nodev,relatime - fuse.gocryptfs /dev/fuse rw,user_id=1026,group_id=100,default_permissions,max_read=131072
13 1 0:200 /tmp /tmp rw,nosuid,nodev,relatime - fuse.gocryptfs /dev/fuse rw,user_id=1026,group_id=100,default_permissions,max_read=131072
14 11 0:200 /gh /home/agent/.config/gh rw,nosuid,nodev,relatime - fuse.gocryptfs /dev/fuse rw,user_id=1026,group_id=100,default_permissions,max_read=131072
`

// An own-host layout: ext4 on a dm-crypt device, tmpfs /tmp.
const luksMountInfo = `1 0 0:90 / / ro,relatime - overlay overlay rw
10 1 253:0 /var/lib/docker/volumes/deck_deck-data/_data /data rw,relatime - ext4 /dev/mapper/cryptroot rw
11 1 253:0 /var/lib/docker/volumes/deck_deck-home/_data /home/agent rw,relatime - ext4 /dev/mapper/cryptroot rw
12 1 253:0 /srv/deck/projects /workspace rw,relatime - ext4 /dev/mapper/cryptroot rw
13 1 0:50 / /tmp rw,nosuid,nodev - tmpfs tmpfs rw,size=2g
`

func mounts(t *testing.T, text string) []Mount {
	t.Helper()
	m, err := ParseMountInfo(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseMountInfo: %v", err)
	}
	return m
}

func dmCryptOn(devices ...string) func(string) string {
	return func(device string) string {
		for _, d := range devices {
			if d == device {
				return "CRYPT-LUKS2-abc-cryptroot"
			}
		}
		return ""
	}
}

func noDM(string) string { return "" }

func byName(results []Result) map[string]Result {
	out := map[string]Result{}
	for _, r := range results {
		out[r.Store.Name] = r
	}
	return out
}

const bootID = "80cfa760-82c5-430b-bcc8-e5fecbd6a51e"
const containerID = "7207dd8c6b40"

// baseFacts is a container that passes every profile-level rule: FUSE
// parameter off, no swap, no hibernation, running as uid 1026.
func baseFacts(t *testing.T, mountInfo string, dm func(string) string) Facts {
	t.Helper()
	return Facts{
		Mounts:                  mounts(t, mountInfo),
		BootID:                  bootID,
		ContainerID:             containerID,
		UID:                     1026,
		DMUUID:                  dm,
		FuseAllowSysAdminAccess: "N",
		ResumeDevice:            "0:0",
	}
}

// reportFor builds a report that matches the mounts exactly, for every
// store and nested mount the check will discover.
func reportFor(t *testing.T, facts Facts, profile Profile, backing Backing) *Report {
	t.Helper()
	rep := &Report{BootID: facts.BootID, ContainerID: facts.ContainerID, Profile: profile, SwapPolicy: facts.SwapPolicy, Stores: map[string]ReportStore{}}
	for _, store := range DefaultStores() {
		m, ok := MountFor(facts.Mounts, store.Path)
		if !ok {
			continue
		}
		b := backing
		if m.FSType == "tmpfs" {
			b = BackingTmpfs
		}
		rep.Stores[store.Name] = ReportStore{Path: store.Path, MountID: m.ID, Device: m.Device, Root: m.Root, Backing: b}
		for _, n := range nestedMounts(facts.Mounts, store.Path) {
			rep.Stores[store.Name+":"+n.MountPoint] = ReportStore{Path: n.MountPoint, MountID: n.ID, Device: n.Device, Root: n.Root, Backing: backing}
		}
	}
	return rep
}

func TestParseMountInfoReadsTheMeasuredLayout(t *testing.T) {
	m := mounts(t, measuredMountInfo)
	data, ok := MountFor(m, "/data")
	if !ok || data.ID != 1437 || data.Device != "0:35" || data.FSType != "btrfs" ||
		data.Root != "/root/var/lib/docker/volumes/deck_deck-data/_data" {
		t.Fatalf("MountFor(/data) = %+v, %v", data, ok)
	}
	nested, ok := MountFor(m, "/home/agent/.mempalace/palace")
	if !ok || nested.ID != 1441 {
		t.Fatalf("MountFor(nested) = %+v, %v", nested, ok)
	}
	root, ok := MountFor(m, "/etc/hostname")
	if !ok || root.ID != 1429 {
		t.Fatalf("MountFor(/etc/hostname) = %+v, %v", root, ok)
	}
}

func TestParseMountInfoUnescapesPaths(t *testing.T) {
	m := mounts(t, `5 1 0:1 / /mnt/with\040space rw - tmpfs tmpfs rw`+"\n")
	if m[0].MountPoint != "/mnt/with space" {
		t.Fatalf("mount point = %q", m[0].MountPoint)
	}
}

// What the container sees alone is never enough when encrypted state is
// required: a gocryptfs layout with no report stays unresolved, because the
// host never qualified it.
func TestGocryptfsLayoutNeedsTheReportWhenRequired(t *testing.T) {
	facts := baseFacts(t, gocryptfsMountInfo, noDM)
	_, err := Check(facts, ProfileShared, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "no host report") {
		t.Fatalf("Check without a report = %v, want a refusal naming the missing report", err)
	}

	facts.Report = reportFor(t, facts, ProfileShared, BackingGocryptfs)
	results, err := Check(facts, ProfileShared, DefaultStores(), true)
	if err != nil {
		t.Fatalf("Check with a matching report: %v", err)
	}
	r := byName(results)
	for _, name := range []string{"data", "home", "projects", "tmp", "home:/home/agent/.config/gh"} {
		if r[name].Backing != BackingGocryptfs || !strings.Contains(r[name].Reason, "verified locally and by the host report") {
			t.Errorf("%s = %+v, want gocryptfs verified by both", name, r[name])
		}
	}
	if !r["home:/home/agent/.config/gh"].Nested {
		t.Error("the gh bind below /home/agent was not discovered as a nested mount")
	}
}

// When not required the same layout is classified and reported, with no
// error, so a user sees what would be refused.
func TestNotRequiredClassifiesWithoutError(t *testing.T) {
	facts := baseFacts(t, gocryptfsMountInfo, noDM)
	results, err := Check(facts, ProfileShared, DefaultStores(), false)
	if err != nil {
		t.Fatalf("Check not required: %v", err)
	}
	if byName(results)["data"].Backing != BackingGocryptfs {
		t.Errorf("relaxed check did not classify locally: %+v", byName(results)["data"])
	}
	facts = baseFacts(t, measuredMountInfo, noDM)
	results, err = Check(facts, ProfileShared, DefaultStores(), false)
	if err != nil {
		t.Fatalf("Check not required on the measured layout: %v", err)
	}
	if byName(results)["data"].Backing != BackingUnresolved {
		t.Errorf("btrfs store should stay unresolved: %+v", byName(results)["data"])
	}
}

func TestProfileFailuresSurfaceWhenNotRequired(t *testing.T) {
	facts := baseFacts(t, gocryptfsMountInfo, noDM)
	facts.ResumeDevice = "253:1"
	if _, err := Check(facts, ProfileOwn, DefaultStores(), false); err != nil {
		t.Fatalf("Check not required: %v", err)
	}
	if got := strings.Join(ProfileFailures(facts, ProfileOwn), "; "); !strings.Contains(got, "hibernation resume device is set") {
		t.Fatalf("ProfileFailures = %q, want the hibernation failure", got)
	}
}

// dm-crypt is the host admin's own unlock and does not count on the
// shared-host profile, from the container's view or from the report.
func TestDMCryptIsRefusedOnSharedHost(t *testing.T) {
	facts := baseFacts(t, luksMountInfo, dmCryptOn("253:0"))
	facts.Report = reportFor(t, facts, ProfileShared, BackingDMCrypt)
	_, err := Check(facts, ProfileShared, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "dm-crypt is not an accepted backing on the shared-host profile") {
		t.Fatalf("Check = %v, want a refusal of dm-crypt on shared host", err)
	}
	// The report alone cannot grant it either: an unresolved store with a
	// report claiming dm-crypt on shared host is plaintext.
	facts = baseFacts(t, luksMountInfo, noDM)
	facts.Report = reportFor(t, facts, ProfileShared, BackingDMCrypt)
	_, err = Check(facts, ProfileShared, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "which the shared-host profile does not accept") {
		t.Fatalf("Check = %v, want the report's dm-crypt refused on shared host", err)
	}
}

func TestLuksLayoutPassesOnOwnHostWithReport(t *testing.T) {
	facts := baseFacts(t, luksMountInfo, dmCryptOn("253:0"))
	facts.Report = reportFor(t, facts, ProfileOwn, BackingDMCrypt)
	results, err := Check(facts, ProfileOwn, DefaultStores(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	r := byName(results)
	for _, name := range []string{"data", "home", "projects"} {
		if r[name].Backing != BackingDMCrypt {
			t.Errorf("%s = %s, want dm-crypt", name, r[name].Backing)
		}
	}
	if r["tmp"].Backing != BackingTmpfs {
		t.Errorf("tmpfs /tmp on own host = %s, want tmpfs", r["tmp"].Backing)
	}
}

// A tmpfs /tmp is readable by host root through /proc/<pid>/root, so the
// shared-host profile refuses it; and tmpfs anywhere but /tmp is refused on
// both profiles.
func TestTmpfsRules(t *testing.T) {
	facts := baseFacts(t, luksMountInfo, dmCryptOn("253:0"))
	facts.Report = reportFor(t, facts, ProfileShared, BackingGocryptfs)
	_, err := Check(facts, ProfileShared, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "tmp (/tmp): plaintext") {
		t.Fatalf("Check = %v, want a refusal naming /tmp as plaintext", err)
	}
	const tmpfsData = luksMountInfo + "20 1 0:51 / /data rw - tmpfs tmpfs rw\n"
	facts = baseFacts(t, tmpfsData, dmCryptOn("253:0"))
	facts.Report = reportFor(t, facts, ProfileOwn, BackingDMCrypt)
	_, err = Check(facts, ProfileOwn, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "tmpfs is accepted only for /tmp") {
		t.Fatalf("Check = %v, want tmpfs /data refused on own host", err)
	}
}

func TestGocryptfsOptionsAreCheckedInTheContainer(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *Facts)
		want   string
	}{
		"allow_other": {func(f *Facts) {
			f.Mounts[1].SuperOpts += ",allow_other"
		}, "allow_other"},
		"allow_root": {func(f *Facts) {
			f.Mounts[1].SuperOpts += ",allow_root"
		}, "allow_root"},
		"root-owned": {func(f *Facts) {
			f.Mounts[1].SuperOpts = strings.Replace(f.Mounts[1].SuperOpts, "user_id=1026", "user_id=0", 1)
		}, "mounted by root"},
		"other uid":        {func(f *Facts) { f.UID = 1000 }, "not this container's uid 1000"},
		"sys_admin access": {func(f *Facts) { f.FuseAllowSysAdminAccess = "Y" }, "allow_sys_admin_access"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			facts := baseFacts(t, gocryptfsMountInfo, noDM)
			tc.mutate(&facts)
			facts.Report = reportFor(t, facts, ProfileShared, BackingGocryptfs)
			_, err := Check(facts, ProfileShared, DefaultStores(), true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Check = %v, want a refusal mentioning %q", err, tc.want)
			}
		})
	}
}

func TestSwapAndHibernationRules(t *testing.T) {
	facts := baseFacts(t, gocryptfsMountInfo, noDM)
	facts.Report = reportFor(t, facts, ProfileShared, BackingGocryptfs)
	facts.Swaps = []string{"/dev/zram0"}
	_, err := Check(facts, ProfileShared, DefaultStores(), true)
	if err != nil {
		t.Fatalf("shared host with swap = %v", err)
	}

	own := baseFacts(t, luksMountInfo, dmCryptOn("253:0", "253:1"))
	own.SwapPolicy = SwapParanoid
	own.Report = reportFor(t, own, ProfileOwn, BackingDMCrypt)
	own.Swaps = []string{"/dev/mapper/cryptswap"}
	own.SwapDevice = func(p string) string {
		if p == "/dev/mapper/cryptswap" {
			return "253:1"
		}
		return "251:0"
	}
	if _, err := Check(own, ProfileOwn, DefaultStores(), true); err != nil {
		t.Fatalf("own host with dm-crypt swap = %v, want pass", err)
	}
	own.Swaps = []string{"/dev/zram0"}
	_, err = Check(own, ProfileOwn, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "swap /dev/zram0 is not verified dm-crypt") {
		t.Fatalf("own host with plain swap = %v", err)
	}
	own.Swaps = nil
	own.ResumeDevice = "253:1"
	_, err = Check(own, ProfileOwn, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "hibernation resume device is set") {
		t.Fatalf("own host with hibernation = %v", err)
	}
}

// A plaintext bind below a sealed store is found and refused, and a sealed
// nested mount is accepted; an intervening mount is never mistaken for the
// store's own mount.
func TestNestedMountsBelowAStoreAreChecked(t *testing.T) {
	const intervening = gocryptfsMountInfo + "15 11 8:1 /cfg /home/agent/.config rw - ext4 /dev/sda1 rw\n"
	facts := baseFacts(t, intervening, noDM)
	facts.Report = reportFor(t, facts, ProfileShared, BackingGocryptfs)
	// The report cannot bless it either: the host would have refused.
	facts.Report.Stores["home:/home/agent/.config"] = ReportStore{Path: "/home/agent/.config", MountID: 15, Device: "8:1", Root: "/cfg", Backing: BackingPlaintext}
	results, err := Check(facts, ProfileShared, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "home:/home/agent/.config (/home/agent/.config): plaintext") {
		t.Fatalf("Check = %v, want the intervening ext4 bind refused by name", err)
	}
	r := byName(results)
	if !r["home:/home/agent/.config"].Nested {
		t.Errorf("intervening mount was not reported as nested: %+v", r["home:/home/agent/.config"])
	}
	// The gh mount below it is still its own gocryptfs mount and passes.
	if r["home:/home/agent/.config/gh"].Backing != BackingGocryptfs {
		t.Errorf("gh mount = %+v, want gocryptfs", r["home:/home/agent/.config/gh"])
	}

	const nestedPalace = luksMountInfo + "16 11 253:0 /srv/palace /home/agent/.mempalace rw - ext4 /dev/mapper/cryptroot rw\n"
	own := baseFacts(t, nestedPalace, dmCryptOn("253:0"))
	own.Report = reportFor(t, own, ProfileOwn, BackingDMCrypt)
	results, err = Check(own, ProfileOwn, DefaultStores(), true)
	if err != nil {
		t.Fatalf("own host with a sealed nested palace = %v", err)
	}
	if p := byName(results)["home:/home/agent/.mempalace"]; !p.Nested || p.Backing != BackingDMCrypt {
		t.Errorf("nested palace = %+v", p)
	}
	// A nested mount the report does not name stays unresolved and refuses.
	delete(own.Report.Stores, "home:/home/agent/.mempalace")
	_, err = Check(own, ProfileOwn, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "host report does not name this store") {
		t.Fatalf("unnamed nested mount = %v", err)
	}
}

// The measured btrfs layout cannot be classified from inside the container:
// with a matching report it is settled, and without one it refuses.
func TestBtrfsLayoutIsSettledOnlyByTheReport(t *testing.T) {
	facts := baseFacts(t, measuredMountInfo, noDM)
	_, err := Check(facts, ProfileOwn, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "data (/data): unresolved") || !strings.Contains(err.Error(), "no host report") {
		t.Fatalf("Check = %v, want unresolved refusal naming the missing report", err)
	}
	facts.Report = reportFor(t, facts, ProfileOwn, BackingDMCrypt)
	// /tmp on the measured layout is ext4 on a loop device; the report says
	// the host verified dm-crypt under it.
	results, err := Check(facts, ProfileOwn, DefaultStores(), true)
	if err != nil {
		t.Fatalf("Check with a matching report: %v", err)
	}
	for _, name := range []string{"data", "home", "projects", "tmp"} {
		r := byName(results)[name]
		if r.Backing != BackingDMCrypt || !strings.Contains(r.Reason, "verified by the host report") {
			t.Errorf("%s = %+v, want dm-crypt verified by the report", name, r)
		}
	}
}

// Every way a report can be stale or for something else leaves the store
// unresolved, and the reason says which comparison failed.
func TestStaleOrForeignReportsAuthorizeNothing(t *testing.T) {
	cases := map[string]struct {
		mutate func(*Report)
		reason string
	}{
		"previous boot":       {func(r *Report) { r.BootID = "00000000-0000-0000-0000-000000000000" }, "another boot"},
		"empty boot id":       {func(r *Report) { r.BootID = "" }, "another boot"},
		"recreated container": {func(r *Report) { r.ContainerID = "deadbeef0000" }, "another container"},
		"other profile":       {func(r *Report) { r.Profile = ProfileShared }, "profile"},
		"replaced mount": {func(r *Report) {
			s := r.Stores["data"]
			s.MountID = 2001
			r.Stores["data"] = s
		}, "different mount"},
		"different device": {func(r *Report) {
			s := r.Stores["data"]
			s.Device = "0:36"
			r.Stores["data"] = s
		}, "different mount"},
		"different root": {func(r *Report) {
			s := r.Stores["data"]
			s.Root = "/root/elsewhere"
			r.Stores["data"] = s
		}, "different mount"},
		"store not named": {func(r *Report) { delete(r.Stores, "data") }, "does not name this store"},
		"store at another path": {func(r *Report) {
			s := r.Stores["data"]
			s.Path = "/data2"
			r.Stores["data"] = s
		}, "different path"},
		"host says plaintext": {func(r *Report) {
			s := r.Stores["data"]
			s.Backing = BackingPlaintext
			r.Stores["data"] = s
		}, "classifies it as plaintext"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			facts := baseFacts(t, measuredMountInfo, noDM)
			rep := reportFor(t, facts, ProfileOwn, BackingDMCrypt)
			tc.mutate(rep)
			facts.Report = rep
			results, err := Check(facts, ProfileOwn, DefaultStores(), true)
			if err == nil {
				t.Fatalf("a %s report was accepted", name)
			}
			data := byName(results)["data"]
			if data.Sealed() || !strings.Contains(data.Reason, tc.reason) {
				t.Errorf("data = %+v, want refused with reason containing %q", data, tc.reason)
			}
		})
	}
}

// The container's own classification and the report must agree; a mount
// the container sees as gocryptfs while the report verified dm-crypt is a
// replaced mount, not a pass.
func TestLocalAndReportDisagreementIsRefused(t *testing.T) {
	facts := baseFacts(t, luksMountInfo, dmCryptOn("253:0"))
	facts.Report = reportFor(t, facts, ProfileOwn, BackingDMCrypt)
	s := facts.Report.Stores["data"]
	s.Backing = BackingGocryptfs
	facts.Report.Stores["data"] = s
	_, err := Check(facts, ProfileOwn, DefaultStores(), true)
	if err == nil || !strings.Contains(err.Error(), "which the own-host profile does not accept") {
		t.Fatalf("Check = %v, want the disagreement refused", err)
	}
}

// A marker file beside the report and an environment variable claiming the
// state is encrypted are not inputs.
func TestMarkerFilesAndEnvironmentAreNeverConsulted(t *testing.T) {
	t.Setenv("NOISEDECK_STATE_IS_ENCRYPTED", "yes")
	t.Setenv("NOISEDECK_SEALED_REQUIRE_ENCRYPTED_STATE", "false")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "encrypted"), []byte("trust me"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts := baseFacts(t, measuredMountInfo, noDM)
	if _, err := Check(facts, ProfileShared, DefaultStores(), true); err == nil {
		t.Fatal("Check passed with only a marker file and an environment claim")
	}
}

func TestReadReportRejectsMalformedJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReport(p); err == nil {
		t.Fatal("ReadReport accepted malformed JSON")
	}
	if _, err := ReadReport(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("ReadReport accepted a missing file")
	}
}

func TestMissingMountIsUnresolved(t *testing.T) {
	facts := Facts{FuseAllowSysAdminAccess: "N"}
	results, err := Check(facts, ProfileShared, []Store{{Name: "data", Path: "/data"}}, true)
	if err == nil {
		t.Fatal("Check passed with no mounts at all")
	}
	if !strings.HasPrefix(byName(results)["data"].Reason, "no mount holds the path") {
		t.Errorf("reason = %q", byName(results)["data"].Reason)
	}
}

func TestReadSwapsSkipsTheHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "swaps")
	if err := os.WriteFile(p, []byte("Filename\tType\tSize\tUsed\tPriority\n/dev/zram0 partition 8388604 0 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts, err := readSwapFacts(p, t.TempDir())
	if err != nil || len(facts.Swaps) != 1 || facts.Swaps[0] != "/dev/zram0" {
		t.Fatalf("swap table: %v %v", facts.Swaps, err)
	}
}

// A configured destination outside the sealed stores, or inside one that is
// not sealed, is refused by name; one inside a sealed store passes.
func TestDestinationFailures(t *testing.T) {
	facts := baseFacts(t, luksMountInfo, dmCryptOn("253:0"))
	facts.Report = reportFor(t, facts, ProfileOwn, BackingDMCrypt)
	results, err := Check(facts, ProfileOwn, DefaultStores(), true)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	failures := DestinationFailures(results, map[string]string{
		"database":      "/data/noisedeck.db",
		"database-lock": "/data/backend.lock",
		"projects root": "/workspace",
		"stray":         "/srv/other/control.db",
	})
	if len(failures) != 1 || !strings.Contains(failures[0], "configured stray (/srv/other/control.db) is outside every sealed store") {
		t.Fatalf("DestinationFailures = %v", failures)
	}

	// Inside a store that failed: named with the store's backing.
	facts = baseFacts(t, luksMountInfo, dmCryptOn("253:0"))
	facts.Report = reportFor(t, facts, ProfileShared, BackingGocryptfs)
	results, _ = Check(facts, ProfileShared, DefaultStores(), true)
	failures = DestinationFailures(results, map[string]string{"database": "/data/noisedeck.db"})
	if len(failures) != 1 || !strings.Contains(failures[0], "is on /data, which is") {
		t.Fatalf("DestinationFailures on a plaintext store = %v", failures)
	}
}

func TestResolveExistingFollowsSymlinksInThePrefix(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got := ResolveExisting(filepath.Join(link, "missing", "control.db"))
	want := filepath.Join(real, "missing", "control.db")
	if resolvedReal, err := filepath.EvalSymlinks(real); err == nil {
		want = filepath.Join(resolvedReal, "missing", "control.db")
	}
	if got != want {
		t.Fatalf("ResolveExisting = %q, want %q", got, want)
	}
}
