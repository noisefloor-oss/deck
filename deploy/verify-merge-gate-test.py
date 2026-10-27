import copy
import importlib.util
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("merge_gate", Path(__file__).with_name("verify-merge-gate.py"))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class ConditionalChecks(unittest.TestCase):
    def setUp(self):
        self.head = "a" * 40
        self.run = {"head_sha": self.head, "path": ".github/workflows/ci-fast.yaml",
                    "event": "pull_request", "status": "completed", "conclusion": "success",
                    "pull_requests": [{"number": 1}]}
        self.checks = [
            {"name": "fast-checks", "conclusion": "SUCCESS", "workflowName": "Fast checks",
             "detailsUrl": "https://github.com/org/deck/actions/runs/7/job/10"},
            {"name": "probes", "conclusion": "SKIPPED", "workflowName": "Fast checks",
             "detailsUrl": "https://github.com/org/deck/actions/runs/7/job/11"},
        ]

    def verify(self, checks=None, run=None):
        with patch.object(gate, "gh", return_value=self.run if run is None else run):
            return gate.check_failures(self.checks if checks is None else checks, self.head, 1, "org", "deck")

    def test_same_run_exact_pr_allows_intentional_skip(self):
        self.assertEqual(self.verify(), [])
        for name in ["source-export", "source-export (parallel)", "probes (gateway)"]:
            self.checks[1]["name"] = name
            self.assertEqual(self.verify(), [])

    def test_failures_pending_and_unknown_skips_still_block(self):
        for state in ["FAILURE", "CANCELLED", "TIMED_OUT", "QUEUED", "IN_PROGRESS", None]:
            checks = copy.deepcopy(self.checks)
            checks[1]["conclusion"] = state
            self.assertTrue(self.verify(checks))
        for name in ["scope", "documentation", "fast-checks", "other-security-check"]:
            checks = copy.deepcopy(self.checks)
            checks[1]["name"] = name
            self.assertTrue(self.verify(checks))
        self.assertTrue(self.verify([]))

    def test_wrong_workflow_head_pr_or_event_blocks(self):
        for key, value in [("head_sha", "b" * 40), ("path", ".github/workflows/fake.yaml"),
                           ("event", "push"), ("status", "in_progress"),
                           ("conclusion", "failure"), ("pull_requests", [{"number": 2}])]:
            run = dict(self.run, **{key: value})
            self.assertTrue(self.verify(run=run))
        checks = copy.deepcopy(self.checks)
        checks[1]["detailsUrl"] = "https://github.com/org/deck/actions/runs/8/job/11"
        self.assertTrue(self.verify(checks))
        for key,value in [("workflowName", "Fake checks"), ("detailsUrl", "https://evil.test/run/7")]:
            checks = copy.deepcopy(self.checks)
            checks[1][key] = value
            with self.assertRaises(RuntimeError): self.verify(checks)

    def test_review_protection_is_not_relaxed(self):
        review = {"author": {"login": "noise-review", "__typename": "Bot"},
                  "state": "APPROVED", "submittedAt": "2026-10-18T00:00:00Z",
                  "body": "APPROVE\n\nReviewed head: " + self.head}
        self.assertEqual(gate.verdict_failures([review], self.head, "noise-review[bot]"), [])
        for change in [{"state": "CHANGES_REQUESTED"}, {"body": "Approved without a marker"},
                       {"body": "Reviewed head: " + "b" * 40},
                       {"author": {"login": "noise-review", "__typename": "User"}}]:
            self.assertTrue(gate.verdict_failures([dict(review, **change)], self.head, "noise-review[bot]"))




class CommitIdentity(unittest.TestCase):
    # GitHub's squash merge lifts every head-commit author it cannot map to
    # the pull request author into a mainline Co-authored-by trailer; these
    # pin the gate that rejects such authorship before it can.

    def commit(self, sha, login):
        return {"sha": sha, "author": ({"login": login} if login else None)}

    def test_every_commit_mapped_to_the_pr_author_passes(self):
        commits = [self.commit("a" * 40, "ddoan"), self.commit("b" * 40, "ddoan")]
        self.assertEqual(gate.commit_identity_failures(commits, "ddoan"), [])

    def test_an_unmapped_agent_identity_is_rejected(self):
        commits = [self.commit("a" * 40, "ddoan"), self.commit("b" * 40, None)]
        failures = gate.commit_identity_failures(commits, "ddoan")
        self.assertEqual(len(failures), 1)
        self.assertIn("unmapped identity", failures[0])
        self.assertIn("Co-authored-by", failures[0])

    def test_a_foreign_account_is_rejected(self):
        failures = gate.commit_identity_failures([self.commit("a" * 40, "someone-else")], "ddoan")
        self.assertEqual(len(failures), 1)
        self.assertIn("someone-else", failures[0])

    def test_missing_commits_or_author_fail_closed(self):
        self.assertTrue(gate.commit_identity_failures([], "ddoan"))
        self.assertTrue(gate.commit_identity_failures(None, "ddoan"))
        self.assertTrue(gate.commit_identity_failures([self.commit("a" * 40, "ddoan")], None))
        self.assertTrue(gate.commit_identity_failures(["not-an-object"], "ddoan"))




class VerdictBaseTests(unittest.TestCase):
    # The approval's envelope names the base it was reviewed against; the
    # gate compares it with the merge base a merge would actually use.

    def review(self, head, base):
        body = f"✅ VERDICT: APPROVE\n\nReviewed head: {head}\nBase: {base}\nReviewer-Model: m"
        return {"state": "APPROVED", "submittedAt": "2026-10-26T00:00:00Z", "body": body,
                "author": {"login": "noise-review", "__typename": "Bot"}}

    def failures(self, base, body_override=None):
        review = self.review("a" * 40, "b" * 40)
        if body_override is not None:
            review["body"] = body_override
        return gate.verdict_failures([review], "a" * 40, "noise-review[bot]", base)

    def test_matching_base_passes(self):
        self.assertEqual(self.failures("b" * 40), [])

    def test_moved_base_blocks(self):
        failures = self.failures("c" * 40)
        self.assertEqual(len(failures), 1)
        self.assertIn("base moved after the review", failures[0])
        self.assertIn("b" * 40, failures[0])
        self.assertIn("c" * 40, failures[0])

    def test_missing_base_marker_blocks_when_base_known(self):
        head = "a" * 40
        body = f"✅ VERDICT: APPROVE\n\nReviewed head: {head}\nReviewer-Model: m"
        failures = self.failures("c" * 40, body_override=body)
        self.assertEqual(len(failures), 1)
        self.assertIn("names base none", failures[0])

    def test_unknown_merge_base_skips_the_base_rule(self):
        # A PR that is not cleanly mergeable has no merge base to compare;
        # the state checks already block it, so the base rule stands down
        # rather than failing on a value that does not exist.
        self.assertEqual(self.failures(""), [])




class CheckPrBaseTests(unittest.TestCase):
    # check_pr's comparison handling: a mergeable PR with an unusable
    # comparison fails closed instead of quietly skipping the base rule.

    def check(self, comparison, mergeable="MERGEABLE"):
        pr = {"state": "OPEN", "isDraft": False, "headRefOid": "a" * 40,
              "baseRefOid": "b" * 40, "mergeable": mergeable,
              "mergeStateStatus": "CLEAN", "statusCheckRollup": [],
              "author": {"login": "ddoan"}}
        head_ok = "a" * 40
        verdict = {"state": "APPROVED", "submittedAt": "2026-10-26T00:00:00Z",
                   "body": f"✅ VERDICT: APPROVE\n\nReviewed head: {head_ok}\nBase: {'b' * 40}\nReviewer-Model: m",
                   "author": {"login": "noise-review", "__typename": "Bot"}}
        # check_pr's gh calls in order: the PR view, the commits list, the
        # compare API; reviews and threads arrive through graphql_nodes.
        calls = iter([pr, [{"sha": "c" * 40, "author": {"login": "ddoan"}}], comparison])
        def fake_gh(*args):
            return next(calls)
        def fake_graphql(connection, *args):
            return [verdict] if connection == "reviews" else []
        with patch.object(gate, "gh", side_effect=fake_gh), \
                patch.object(gate, "graphql_nodes", side_effect=fake_graphql):
            return gate.check_pr(1, "noise-review[bot]", "org", "deck")

    def test_valid_comparison_passes(self):
        good = {"merge_base_commit": {"sha": "b" * 40}}
        failures = self.check(good)
        self.assertTrue(all("merge-base" not in f for f in failures), failures)

    def test_missing_sha_fails_closed(self):
        failures = self.check({})
        self.assertTrue(any("no valid 40-character" in f for f in failures), failures)

    def test_null_sha_fails_closed(self):
        failures = self.check({"merge_base_commit": {"sha": None}})
        self.assertTrue(any("no valid 40-character" in f for f in failures), failures)

    def test_short_sha_fails_closed(self):
        failures = self.check({"merge_base_commit": {"sha": "b" * 12}})
        self.assertTrue(any("no valid 40-character" in f for f in failures), failures)

    def test_nonmergeable_skips_comparison(self):
        # A non-mergeable PR never makes the comparison call; the state
        # checks already block it.
        failures = self.check(None, mergeable="CONFLICTING")
        self.assertFalse(any("merge-base" in f for f in failures), failures)


if __name__ == "__main__":
    unittest.main()
