import importlib.util
import io
import json
import zipfile
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("preflight", Path(__file__).with_name("release-preflight.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class PreflightTests(unittest.TestCase):
    def manifest(self):
        return dict(version=1, repository="private/deck", sha="a" * 40, owner="release-owner", scope=["security update"])

    def test_missing_evidence_reports_all_blockers(self):
        def fetch(path, pages):
            if "/compare/" in path: return {"status": "ahead"}
            if path.endswith("commits/main"): return {"sha": "b" * 40}
            if pages: return [{"workflow_runs": [], "runners": []}]
            if "actions/workflows" in path: return {"state": "disabled_manually"}
            return {"private": True}
        report = module.inspect(self.manifest(), fetch)
        self.assertFalse(report["ready"])
        self.assertTrue(report["main_advanced"])
        self.assertEqual(len([b for b in report["blockers"] if "no current successful" in b]), 3)
        self.assertEqual(len([b for b in report["blockers"] if "no online native" in b]), 2)

    def test_other_heads_repositories_branches_cannot_supply_evidence(self):
        run = dict(id=1, event="workflow_dispatch", head_sha="a" * 40, head_branch="main", path=".github/workflows/ci-fast.yaml", head_repository={"full_name": "private/deck"})
        self.assertEqual(module.matching_run([run], "private/deck", "a"*40, "ci-fast.yaml"), run)
        for changed in (dict(head_sha="b"*40), dict(head_branch="feature"), dict(head_repository={"full_name":"other/deck"}), dict(path="other.yaml")):
            self.assertIsNone(module.matching_run([dict(run, **changed)], "private/deck", "a"*40, "ci-fast.yaml"))

    def test_latest_failure_supersedes_earlier_success(self):
        run = dict(id=1, event="workflow_dispatch", head_sha="a" * 40, head_branch="main", path=".github/workflows/ci-fast.yaml", head_repository={"full_name": "private/deck"}, conclusion="success")
        self.assertEqual(module.matching_run([run, dict(run, id=2, conclusion="failure")], "private/deck", "a"*40, "ci-fast.yaml")["conclusion"], "failure")

    def test_selected_org_group_contributes_only_eligible_runners(self):
        def fetch(path, pages):
            if "/compare/" in path: return {"status": "identical"}
            if path.endswith("commits/main"): return {"sha": "a" * 40}
            if "/runner-groups?" in path: return [{"runner_groups": [dict(id=1, visibility="selected"), dict(id=2, visibility="selected")]}]
            if "/runner-groups/1/repositories" in path: return [{"repositories": [{"full_name": "private/deck"}]}]
            if "/runner-groups/2/repositories" in path: return [{"repositories": [{"full_name": "other/deck"}]}]
            if "/runner-groups/2/runners" in path: self.fail("ineligible group must not contribute runners")
            if "/runner-groups/1/runners" in path:
                return [{"runners": [dict(name="native-"+arch, status="online", labels=[dict(name=n) for n in ("self-hosted", "Linux", arch, "native-linux")]) for arch in ("ARM64", "X64")]}]
            if pages: return [{"workflow_runs": [], "runners": []}]
            return {"private": True, "state": "active"}
        report = module.inspect(self.manifest(), fetch)
        self.assertFalse(any("no online native" in b for b in report["blockers"]))

    def test_selective_pr_cannot_qualify_candidate(self):
        run = dict(id=1, event="pull_request", head_sha="a"*40, head_branch="main", path=".github/workflows/ci-fast.yaml", head_repository={"full_name":"private/deck"})
        self.assertIsNone(module.matching_run([run], "private/deck", "a"*40, "ci-fast.yaml"))

    def queue_run(self):
        return dict(id=42, run_attempt=2, event="merge_group", head_sha="a"*40,
                    head_branch="gh-readonly-queue/main/pr-1-" + "b"*40,
                    path=".github/workflows/ci-fast.yaml", head_repository={"full_name":"private/deck"},
                    status="completed", conclusion="success")

    def full_jobs(self):
        return [dict(id=i, name=name, run_id=42, run_attempt=2, head_sha="a"*40,
                     status="completed", conclusion="success")
                for i, name in enumerate(sorted(module.FULL_FAST_JOBS), 1)]

    def test_full_queue_requires_every_matrix_lane_and_attempt(self):
        jobs = self.full_jobs()
        paths = []
        def fetch(path, pages):
            paths.append(path)
            self.assertTrue(pages)
            return [{"jobs":jobs[:10]}, {"jobs":jobs[10:]}]
        def proof(): return module.full_queue_proof("private/deck", "a"*40, self.queue_run(), fetch)
        self.assertEqual(proof()["missing"], [])
        self.assertIn("/runs/42/attempts/2/jobs?per_page=100", paths[0])
        self.assertEqual(len(proof()["covered"]), 18)
        for field, value in [("conclusion","skipped"), ("conclusion","failure"), ("status","in_progress"),
                             ("head_sha","b"*40), ("run_id",41), ("run_attempt",1)]:
            jobs = self.full_jobs()
            jobs[0][field] = value
            self.assertTrue(proof()["missing"], (field, value))
        jobs = self.full_jobs()[1:]
        self.assertTrue(proof()["missing"])
        jobs = self.full_jobs()
        jobs.append(dict(jobs[0]))
        self.assertTrue(proof()["missing"])
        jobs = self.full_jobs() + [dict(name="new-unknown-suite")]
        self.assertEqual(proof()["unexpected"], ["new-unknown-suite"])

    def test_queue_identity_and_selective_job_coverage(self):
        run = self.queue_run()
        self.assertEqual(module.matching_run([run], "private/deck", "a"*40, "ci-fast.yaml"), run)
        for fields in [dict(head_branch="gh-readonly-queue/other/pr-1"), dict(head_sha="b"*40),
                       dict(head_repository={"full_name":"other/deck"}), dict(event="pull_request")]:
            self.assertIsNone(module.matching_run([dict(run, **fields)], "private/deck", "a"*40, "ci-fast.yaml"))
        jobs = [j for j in self.full_jobs() if j["name"] in {"scope", "documentation", "fast-checks"}]
        proof = module.full_queue_proof("private/deck", "a"*40, run, lambda path, pages: [{"jobs":jobs}])
        self.assertIn("probes (3)", proof["missing"])
        self.assertIn("source-export (serialized)", proof["missing"])

    def test_inspection_reuses_full_queue_but_rejects_partial_or_failed_run(self):
        run, jobs = self.queue_run(), self.full_jobs()
        def fetch(path, pages):
            if "/compare/" in path: return {"status":"ahead"}
            if path.endswith("commits/main"): return {"sha":"b"*40}
            if "/ci-fast.yaml/runs?" in path:
                self.assertNotIn("branch=main", path)
                return [{"workflow_runs":[run]}]
            if "/attempts/2/jobs?" in path: return [{"jobs":jobs}]
            if pages: return []
            return {"private":True, "state":"active"}
        def fast_blockers():
            return [b for b in module.inspect(self.manifest(), fetch)["blockers"] if b.startswith("ci-fast.yaml:")]
        self.assertEqual(fast_blockers(), [])
        jobs = self.full_jobs()[1:]
        self.assertTrue(fast_blockers())
        jobs = self.full_jobs() + [dict(name="unrecognized")]
        self.assertTrue(fast_blockers())
        jobs = self.full_jobs()
        run["conclusion"] = "failure"
        self.assertTrue(fast_blockers())

    def test_publication_download_can_use_operator_transport(self):
        record = dict(version=1, repository="private/deck", sha="a"*40, run="42", attempt="2",
                      image_set=dict(sha="a"*40, images={role:"ghcr.io/private/image@sha256:"+"b"*64 for role in ("free", "browser")}))
        buffer = io.BytesIO()
        with zipfile.ZipFile(buffer, "w") as archive:
            archive.writestr("release-publication.json", json.dumps(record))
        paths = []
        def fetch(path, pages):
            self.assertEqual(path, "repos/private/deck/actions/runs/42/artifacts?per_page=100")
            return [{"artifacts":[dict(id=9, name="release-publication-2", size_in_bytes=len(buffer.getvalue()), expired=False)]}]
        def download(path):
            paths.append(path)
            return buffer.getvalue()
        self.assertEqual(module.publication_proof("private/deck", "a"*40, dict(id=42,run_attempt=2), fetch, download), record)
        self.assertEqual(paths, ["repos/private/deck/actions/artifacts/9/zip"])

    def test_publication_requires_candidate_attempt_and_digests(self):
        run = dict(id=42, run_attempt=2)
        record = dict(version=1, repository="private/deck", sha="a"*40, run="42", attempt="2", image_set=dict(sha="a"*40, images={role:"ghcr.io/private/image@sha256:"+"b"*64 for role in ("free", "browser")}))
        self.assertTrue(module.valid_publication(record, "private/deck", "a"*40, run))
        for patch in (dict(sha="c"*40), dict(attempt="1"), dict(run="41"), dict(image_set=None), dict(image_set=dict(sha="a"*40,images={"free":"mutable:latest","browser":"mutable:latest"}))):
            self.assertFalse(module.valid_publication(dict(record, **patch), "private/deck", "a"*40, run))

    def test_successful_image_run_without_publication_proof_blocks(self):
        def fetch(path, pages):
            if "/compare/" in path: return {"status": "identical"}
            if path.endswith("commits/main"): return {"sha": "a"*40}
            if "/actions/workflows/" in path and pages:
                workflow = path.split("/actions/workflows/")[1].split("/")[0]
                return [{"workflow_runs": [dict(id=42, run_attempt=1, event="workflow_dispatch", head_sha="a"*40, head_branch="main", path=".github/workflows/"+workflow, head_repository={"full_name":"private/deck"}, status="completed", conclusion="success")]}]
            if "/actions/runners?" in path:
                return [{"runners": [dict(status="online", labels=[dict(name=n) for n in ("self-hosted", "Linux", arch, "native-linux")]) for arch in ("ARM64", "X64")]}]
            if pages: return []
            return {"private": True, "state": "active"}
        with patch.object(module, "publication_proof", return_value=None):
            report = module.inspect(self.manifest(), fetch)
            self.assertFalse(report["ready"])
            self.assertTrue(any("publication unverified" in b for b in report["blockers"]))
        with patch.object(module, "publication_proof", return_value={"verified": True}):
            self.assertTrue(module.inspect(self.manifest(), fetch)["ready"])
        def newer_candidate(path, pages):
            result = fetch(path, pages)
            if "release-images.yaml/runs?" in path:
                result[0]["workflow_runs"].append(dict(result[0]["workflow_runs"][0], id=43, head_sha="b"*40))
            return result
        with patch.object(module, "publication_proof", side_effect=lambda repo, sha, run, fetch: {"verified": True} if run["id"] == 42 else None) as proof:
            report = module.inspect(self.manifest(), newer_candidate)
            self.assertTrue(report["ready"])
            self.assertEqual(report["evidence"]["release-images.yaml qualification"]["id"], 42)
            self.assertEqual([c.args[2]["id"] for c in proof.call_args_list], [43, 42])

    def test_missing_owner_scope_or_sha_refused(self):
        for key, value in (("owner", ""), ("scope", []), ("sha", "main")):
            with self.assertRaises(ValueError): module.validate(dict(self.manifest(), **{key:value}))


if __name__ == "__main__": unittest.main()
