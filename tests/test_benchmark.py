"""Exercise the benchmark driver with small, controlled command substitutes."""

import os
import pathlib
import subprocess
import sys
import tempfile
import unittest


@unittest.skipUnless(os.name == "posix", "the benchmark uses Unix tools")
class BenchmarkTest(unittest.TestCase):
    def test_failures_and_existing_output(self):
        script = pathlib.Path(__file__).resolve().parents[1] / "benchmark.py"
        for failure in (None, "parallel", "seq"):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory(prefix="rush benchmark ") as directory:
                work = pathlib.Path(directory)
                tools = work / "bin"
                tools.mkdir()
                for name in ("parallel", "rust-parallel", "gargs", "rush", "seq"):
                    command = "printf 'input\\n'" if name == "seq" else "cat >/dev/null; printf 'output\\n'"
                    if name == failure:
                        command = "exit 7"
                    path = tools / name
                    path.write_text("#!/bin/sh\n" + command + "\n")
                    path.chmod(0o700)
                existing = work / "t.parallel"
                existing.write_text("existing data")
                result = subprocess.run(
                    [sys.executable, str(script)], cwd=work,
                    env={**os.environ, "PATH": str(tools) + os.pathsep + os.defpath, "TMPDIR": str(work)},
                    capture_output=True, text=True, timeout=10,
                )
                self.assertEqual(existing.read_text(), "existing data")
                self.assertEqual(list(work.glob("rush-benchmark-*")), [])
                if failure:
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("\ntime:\n", result.stderr)
                else:
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertIn("\ntime:\n", result.stderr)


if __name__ == "__main__":
    unittest.main()
