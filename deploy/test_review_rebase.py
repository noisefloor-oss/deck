import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("rebase", Path(__file__).with_name("review-rebase.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class RebaseTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="rr-")
        self.previous = os.getcwd()
        os.chdir(self.directory.name)
        self.git("init", "-q")
        Path("file").write_text("before\n")
        self.base = self.save()
        Path("file").write_text("after\n")
        self.old = self.save()
        self.git("checkout", "--detach", self.base)
        Path("unrelated").write_text("base change\n")
        self.newbase = self.save()
        Path("file").write_text("after\n")
        self.new = self.save()

    def tearDown(self):
        os.chdir(self.previous)
        self.directory.cleanup()

    def git(self, *args):
        return subprocess.check_output(["git", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", *args], stderr=subprocess.DEVNULL).decode().strip()

    def save(self):
        self.git("add", ".")
        self.git("commit", "-qm", "fixture")
        return self.git("rev-parse", "HEAD")

    def test_disjoint_base_change_preserves_exact_patch(self):
        result = module.compare(self.base, self.old, self.newbase, self.new)
        self.assertTrue(result["eligible_for_bounded_review"])
        self.assertEqual(result["old_merge_base"], self.base)
        self.assertEqual(result["new_merge_base"], self.newbase)
        self.assertIn("Independently", result["requirements"][1])

    def test_whitespace_and_mode_changes_require_full_review(self):
        Path("file").write_text("after \n")
        self.assertFalse(module.compare(self.base, self.old, self.newbase, self.save())["eligible_for_bounded_review"])
        Path("file").write_text("after\n")
        Path("file").chmod(0o755)
        self.assertFalse(module.compare(self.base, self.old, self.newbase, self.save())["eligible_for_bounded_review"])

    def test_symlink_changes_require_full_review(self):
        os.symlink("file", "link")
        self.assertFalse(module.compare(self.base, self.old, self.newbase, self.save())["eligible_for_bounded_review"])

    def test_submodule_ignore_config_cannot_hide_changes(self):
        self.git("update-index", "--add", "--cacheinfo", "160000,"+self.base+",module")
        self.git("commit", "-qm", "gitlink")
        new = self.git("rev-parse", "HEAD")
        self.git("config", "diff.ignoreSubmodules", "all")
        self.assertFalse(module.compare(self.base, self.old, self.newbase, new)["eligible_for_bounded_review"])

    def test_relative_config_cannot_hide_changes_or_base_delta(self):
        Path("extra").write_text("changed at root\n")
        new = self.save()
        self.git("config", "diff.relative", "true")
        Path("subdirectory").mkdir()
        os.chdir("subdirectory")
        self.assertFalse(module.compare(self.base, self.old, self.newbase, new)["eligible_for_bounded_review"])
        self.assertIn(b"base change", module.git("diff", self.base, self.newbase))

    def test_moving_refs_rejected(self):
        with self.assertRaises(ValueError): module.compare("HEAD", self.old, self.newbase, self.new)


if __name__ == "__main__": unittest.main()
