import unittest
from unittest.mock import patch
import subprocess
from job_timings import summarize, phase, markdown, fetch_jobs


class TimingTests(unittest.TestCase):
    def test_pending_execution_and_skips(self):
        job = dict(created_at="2026-10-25T00:00:00Z", started_at="2026-10-25T00:01:00Z", completed_at="2026-10-25T00:02:00Z", steps=[
            dict(name="Install Go", status="completed", conclusion="success", started_at="2026-10-25T00:01:00Z", completed_at="2026-10-25T00:01:10Z"),
            dict(name="Run tests", status="completed", conclusion="success", started_at="2026-10-25T00:01:10Z", completed_at="2026-10-25T00:01:50Z"),
            dict(name="Run skipped", status="completed", conclusion="skipped", started_at="2026-10-25T00:01:10Z", completed_at="2026-10-25T00:01:50Z")])
        row = summarize(job)
        self.assertEqual(row["pending_seconds"], 60)
        self.assertEqual(row["step_seconds"]["execution"], 40)
        self.assertEqual(row["step_seconds"]["setup_cache"], 10)
        self.assertEqual(row["unattributed_seconds"], 10)
        self.assertEqual(row["total_seconds"], 120)

    def test_unassigned_and_unknown(self):
        self.assertIsNone(summarize({})["pending_seconds"])
        self.assertIsNone(summarize({})["wall_seconds"])
        self.assertEqual(phase("Mystery operation"), "unclassified")
        self.assertEqual(phase("Check out source"), "setup_cache")
        self.assertEqual(phase("Post Install Go"), "cleanup_artifacts")

    @patch("job_timings.subprocess.check_output")
    def test_paginated_jobs_use_compatible_cli(self, fetch):
        fetch.return_value = '{"id":1,"name":"first\\njob"}\n{"id":2}\n'
        self.assertEqual([job["id"] for job in fetch_jobs("o/r", "123", "2")], [1, 2])
        argv = fetch.call_args.args[0]
        self.assertIn("--paginate", argv)
        self.assertNotIn("--slurp", argv)
        self.assertIn("repos/o/r/actions/runs/123/attempts/2/jobs?per_page=100", argv)
        fetch.return_value = '{"id":3,"name":"line\u2028separator\u2029label"}\n'
        self.assertEqual(fetch_jobs("o/r", "123", "2")[0]["name"], "line\u2028separator\u2029label")
        fetch.return_value = ""
        self.assertEqual(fetch_jobs("o/r", "123", "2"), [])
        fetch.side_effect = subprocess.CalledProcessError(1, argv)
        with self.assertRaises(subprocess.CalledProcessError):
            fetch_jobs("o/r", "123", "2")

    def test_escaped_labels(self):
        text = markdown([summarize(dict(name="<script>|bad\nname"))])
        self.assertNotIn("<script>", text)
        self.assertIn("&lt;script&gt;", text)


if __name__ == "__main__":
    unittest.main()
