import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import ci_scope
import jev_plan


class ScopeTests(unittest.TestCase):
    def test_documentation_only(self):
        result = ci_scope.classify([
            "README.md", "skills/install-deck/SKILL.md",
            "skills/install-deck/references/native-macos.md", "docs/guide.md",
        ])
        self.assertEqual(result["mode"], "selective")
        self.assertEqual(result["jobs"], ["documentation"])

    def test_frontend_and_backend(self):
        for paths, expected in [
            (["web/src/app/live.ts"], ci_scope.WEB),
            (["web/public/icon.svg"], ci_scope.WEB),
            (["internal/history/launch.go"], ci_scope.GO),
            (["cmd/noisedeck/main_test.go"], ci_scope.GO),
            (["web/src/app/live.ts", "internal/history/launch.go"], ci_scope.WEB | ci_scope.GO),
        ]:
            with self.subTest(paths=paths):
                result = ci_scope.classify(paths)
                self.assertEqual(set(result["jobs"]), expected | {"documentation"})
                self.assertEqual(result["mode"], "selective")

    def test_unknown_and_shared_build_changes_are_full(self):
        for path in [
            ".noise/review-focus.md", ".gitattributes",
            "Dockerfile", "compose.yaml", "go.mod", "go.sum", "Makefile",
            "new-component/config.toml", "docs/picture.png", "./README.md",
            "../README.md", "/README.md", "docs/a\nb.md",
        ]:
            with self.subTest(path=path):
                self.assertEqual(ci_scope.classify([path])["mode"], "full")
                self.assertEqual(ci_scope.classify(["README.md", path])["mode"], "full")
        self.assertEqual(ci_scope.classify([])["mode"], "full")

    def test_scoped_operations_and_security(self):
        for path, expected in [
            (".github/scripts/ci_scope.py", {"deploy-suite"}),
            ("SDLC.md", {"deploy-suite"}),
            ("deploy/export/export.go", ci_scope.GO | {"deploy-suite", "source-export"}),
            ("deploy/derived/go_recipe_test.go", ci_scope.GO | {"deploy-suite"}),
            ("scripts/provider-spike/conformance/codex_test.go", ci_scope.GO | {"deploy-suite"}),
            ("deploy/compose.env.example", {"deploy-suite"}),
            ("web/package.json", ci_scope.WEB | {"deploy-suite"}),
            ("web/pnpm-lock.yaml", ci_scope.WEB | {"deploy-suite"}),
            ("web/tools/security-go-probe.mjs", ci_scope.WEB | {"deploy-suite"}),
            ("web/tools/onboarding-go-probe.mjs", ci_scope.WEB | {"deploy-suite"}),
            ("docs/security/confined-linux-v1.md", ci_scope.GO | {"deploy-suite"}),
            ("docs/adr/0015-model-refresh.md", ci_scope.GO | {"deploy-suite"}),
            ("docs/provider-contracts/README.md", ci_scope.GO | {"deploy-suite"}),
            ("docs/PRINCIPLES.md", ci_scope.GO | {"deploy-suite"}),
            ("web/tools/assets-news-probe.mjs", ci_scope.WEB),
            ("internal/auth/password.go", ci_scope.GO | ci_scope.WEB),
            ("internal/ciheal/controller.go", ci_scope.GO | ci_scope.WEB | {"jail-probe", "deploy-suite"}),
        ]:
            with self.subTest(path=path):
                result = ci_scope.classify([path])
                self.assertEqual(result["mode"], "selective")
                self.assertEqual(set(result["jobs"]), expected | {"documentation"})

    def test_workflow_change_is_full(self):
        for path in [".github/workflows/ci-fast.yaml", ".github/workflows/harness-bump.yaml"]:
            with self.subTest(path=path):
                self.assertEqual(ci_scope.classify(["README.md", path])["mode"], "full")

    def test_browser_lanes(self):
        for paths in [["web/src/app/live.ts"], ["web/tools/assets-news-probe.mjs"]]:
            self.assertEqual(ci_scope.classify(paths)["probe_shards"], [1, 2, 3])
        for path in ["web/src/security/client.ts", "web/src/app/security/enroll.ts", "internal/auth/password.go", "web/pnpm-lock.yaml", "web/tools/security-go-probe.mjs"]:
            self.assertEqual(ci_scope.classify([path])["probe_shards"], ci_scope.PROBE_SHARDS)
        self.assertEqual(ci_scope.full("manual")["probe_shards"], ci_scope.PROBE_SHARDS)

    def test_non_pr_and_fork_are_full(self):
        for event in ["merge_group", "push", "workflow_dispatch", "unknown"]:
            self.assertEqual(ci_scope.plan(event, {})["mode"], "full")
        event = {"pull_request": {
            "base": {"repo": {"full_name": "org/deck"}},
            "head": {"repo": {"full_name": "fork/deck"}},
        }}
        self.assertEqual(ci_scope.plan("pull_request", event)["mode"], "full")
        for malformed in [{}, {"pull_request": None}, {"pull_request": {}}]:
            self.assertEqual(ci_scope.plan("pull_request", malformed)["mode"], "full")


class GitDiffTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.previous = os.getcwd()
        os.chdir(self.temp.name)
        self.addCleanup(os.chdir, self.previous)
        self.env = dict(os.environ, GIT_AUTHOR_NAME="CI fixture", GIT_COMMITTER_NAME="CI fixture",
                        GIT_AUTHOR_EMAIL="ci@example.invalid", GIT_COMMITTER_EMAIL="ci@example.invalid")
        self.git("init", "-q")
        self.git("symbolic-ref", "HEAD", "refs/heads/main")
        Path("README.md").write_text("# Example\n")
        self.base = self.commit()

    def git(self, *args):
        return subprocess.check_output(["git", *args], env=self.env,
                                       stderr=subprocess.DEVNULL, text=True).strip()

    def commit(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")
        return self.git("rev-parse", "HEAD")

    def plan(self, head):
        event = {"pull_request": {
            "base": {"sha": self.base, "repo": {"full_name": "org/deck"}},
            "head": {"sha": head, "repo": {"full_name": "org/deck"}},
        }}
        return ci_scope.plan("pull_request", event)

    def test_real_documentation_diff(self):
        Path("README.md").write_text("# Changed\n")
        self.assertEqual(self.plan(self.commit())["jobs"], ["documentation"])

    def test_merge_group_uses_entire_base_to_head_diff(self):
        Path("web/src").mkdir(parents=True)
        Path("web/src/app.ts").write_text("export {};\n")
        self.commit()
        Path("README.md").write_text("# Queue docs\n")
        head = self.commit()
        event = {"merge_group": {"base_sha": self.base, "head_sha": head}}
        result = ci_scope.plan("merge_group", event)
        self.assertEqual(result["mode"], "selective")
        self.assertEqual(set(result["jobs"]), ci_scope.WEB | {"documentation"})
        event["merge_group"]["head_sha"] = "0" * 40
        self.assertEqual(ci_scope.plan("merge_group", event)["mode"], "full")

    def test_rename_checks_old_and_new_paths(self):
        Path("docs").mkdir()
        self.git("mv", "README.md", "docs/guide.md")
        head = self.commit()
        self.assertEqual(set(ci_scope.changed_paths(self.base, head)), {"README.md", "docs/guide.md"})
        self.assertEqual(self.plan(head)["jobs"], ["documentation"])
        Path("internal").mkdir()
        self.git("mv", "docs/guide.md", "internal/guide.go")
        self.assertEqual(set(self.plan(self.commit())["jobs"]), ci_scope.GO | {"documentation"})

    def test_entire_pr_diff_not_last_commit(self):
        Path("Dockerfile").write_text("FROM scratch\n")
        self.commit()
        Path("README.md").write_text("# Later docs\n")
        self.assertEqual(self.plan(self.commit())["mode"], "full")

    def test_base_only_changes_do_not_enter_pr_diff(self):
        Path("README.md").write_text("# Branch docs\n")
        head = self.commit()
        self.git("checkout", "-q", "-b", "base-advance", self.base)
        Path("Dockerfile").write_text("FROM scratch\n")
        self.base = self.commit()
        self.assertEqual(self.plan(head)["jobs"], ["documentation"])

    def test_full_rule_plan_is_not_narrowed_by_jev(self):
        Path("go.mod").write_text("module example\n")
        head = self.commit()
        event = {"repository": {"private": True}, "merge_group": {"base_sha": self.base, "head_sha": head}}
        narrowed = {"mode": "selective", "jobs": ["documentation"], "probe_shards": [1, 2, 3], "selector": "jev"}
        with patch.dict(os.environ, GITHUB_RUN_ID="7", GITHUB_REPOSITORY="org/deck"), \
                patch.object(jev_plan, "load", return_value=narrowed) as load:
            selected = ci_scope.select_plan("merge_group", event)
        self.assertEqual(selected["mode"], "full")
        self.assertEqual(selected["jobs"], list(ci_scope.SUITES))
        load.assert_not_called()

    def merge_group_plan(self, path, narrowed):
        Path(path).parent.mkdir(parents=True, exist_ok=True)
        Path(path).write_text("x\n")
        head = self.commit()
        event = {"repository": {"private": True}, "merge_group": {"base_sha": self.base, "head_sha": head}}
        with patch.dict(os.environ, GITHUB_RUN_ID="7", GITHUB_REPOSITORY="org/deck"), \
                patch.object(jev_plan, "load", return_value=dict(narrowed, selector="jev")):
            return ci_scope.select_plan("merge_group", event)

    def test_jev_cannot_drop_the_security_ui_floor(self):
        docs_only = {"mode": "selective", "jobs": ["documentation"], "probe_shards": [1, 2, 3]}
        selected = self.merge_group_plan("web/src/app/security/panel.ts", docs_only)
        self.assertEqual(selected["selector"], "jev")
        self.assertEqual(selected["jobs"], ["documentation", "frontend", "probes"])
        self.assertEqual(selected["probe_shards"], ci_scope.PROBE_SHARDS)
        ui_only = {"mode": "selective", "jobs": ["documentation", "frontend", "probes"], "probe_shards": [1, 2, 3]}
        selected = self.merge_group_plan("web/src/app/security/other.ts", ui_only)
        self.assertEqual(selected["probe_shards"], ci_scope.PROBE_SHARDS)

    def test_go_mod_stays_full_with_a_narrowing_jev_plan(self):
        docs_only = {"mode": "selective", "jobs": ["documentation"], "probe_shards": [1, 2, 3]}
        selected = self.merge_group_plan("go.mod", docs_only)
        self.assertEqual(selected["mode"], "full")
        self.assertEqual(selected["jobs"], list(ci_scope.SUITES))

    def test_jev_cannot_narrow_an_ordinary_code_path(self):
        # The advisor once read a help-screen and PTY-fixture change as
        # docs-only and the affected code suites never ran; the
        # deterministic classification of every changed path is now the
        # floor, so a docs-only plan cannot drop the suites the diff's own
        # paths demand. Jev still selects - it can only add.
        docs_only = {"mode": "selective", "jobs": ["documentation"], "probe_shards": [1, 2, 3]}
        selected = self.merge_group_plan("web/src/app/widget.ts", docs_only)
        self.assertEqual(selected["selector"], "jev")
        self.assertEqual(selected["jobs"], ["documentation", "frontend", "probes"])
        Path("web/src/app/widget.ts").unlink()
        go_docs_only = {"mode": "selective", "jobs": ["documentation"], "probe_shards": [1, 2, 3]}
        selected = self.merge_group_plan("internal/tmux/help.go", go_docs_only)
        self.assertEqual(set(selected["jobs"]), ci_scope.GO | {"documentation"})
        Path("internal/tmux/help.go").unlink()
        docs_change = {"mode": "selective", "jobs": ["documentation"], "probe_shards": [1, 2, 3]}
        selected = self.merge_group_plan("docs/guide.md", docs_change)
        self.assertEqual(selected["jobs"], ["documentation"], "a docs-only diff keeps the docs-only plan")

    def test_deletion_modes_symlinks_and_missing_commits(self):
        Path("README.md").unlink()
        self.assertEqual(self.plan(self.commit())["jobs"], ["documentation"])
        Path("README.md").symlink_to("Dockerfile")
        self.assertEqual(self.plan(self.commit())["mode"], "full")
        self.assertEqual(self.plan("0" * 40)["mode"], "full")
        self.assertEqual(self.plan("--not-a-commit")["mode"], "full")

    def test_executable_markdown_is_not_docs_only(self):
        Path("README.md").chmod(0o755)
        self.assertEqual(self.plan(self.commit())["mode"], "full")


class OutputTests(unittest.TestCase):
    def test_outputs_and_summary_are_explicit(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "output"
            summary = Path(directory) / "summary"
            with patch.dict(os.environ, GITHUB_OUTPUT=str(output), GITHUB_STEP_SUMMARY=str(summary)):
                ci_scope.publish(ci_scope.classify(["README.md"]))
            lines = dict(line.split("=", 1) for line in output.read_text().splitlines())
            self.assertEqual(lines["mode"], "selective")
            self.assertEqual(json.loads(lines["jobs"]), ["documentation"])
            self.assertIn("Selected checks: documentation", summary.read_text())


if __name__ == "__main__":
    unittest.main()
