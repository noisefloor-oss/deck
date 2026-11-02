import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("ci_capacity.sh").resolve()


class CapacityTests(unittest.TestCase):
    def test_cpu_count_is_bounded_and_invalid_counts_fail_closed(self):
        with tempfile.TemporaryDirectory() as directory:
            stub = Path(directory) / "nproc"
            stub.write_text('#!/bin/sh\nprintf "%s\\n" "$TEST_CPUS"\n')
            stub.chmod(0o755)
            for cpus, expected in (("1", "1"), ("2", "2"), ("4", "4"), ("12", "4"),
                                   ("0", None), ("-1", None), ("oops", None), ("", None)):
                with self.subTest(cpus=cpus):
                    env = dict(os.environ, PATH=directory + ":" + os.environ["PATH"], TEST_CPUS=cpus)
                    result = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True)
                    if expected is None:
                        self.assertNotEqual(result.returncode, 0)
                        self.assertEqual(result.stdout, "")
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertEqual(result.stdout.strip(), expected)


if __name__ == "__main__":
    unittest.main()
