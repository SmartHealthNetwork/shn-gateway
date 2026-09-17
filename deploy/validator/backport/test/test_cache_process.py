import importlib.util
import os
from pathlib import Path
import sys
import tempfile
import time
import unittest
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("cache", Path(__file__).parents[1] / "cache.py")
cache = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cache)


class BuildProcessTest(unittest.TestCase):
    def test_actual_exit(self):
        self.assertEqual(cache.run_build([sys.executable, "-c", "raise SystemExit(17)"], 10), 17)

    def test_timeout_stops_and_joins_group(self):
        with tempfile.TemporaryDirectory() as directory:
            marker = Path(directory) / "writes"
            program = "import time,pathlib; p=pathlib.Path(" + repr(str(marker)) + "); " \
                "exec('while True:\\n with p.open(\\\"a\\\") as f: f.write(\\\"x\\\")\\n time.sleep(0.01)')"
            with self.assertRaises(TimeoutError):
                cache.run_build([sys.executable, "-c", program], 0.15)
            size = marker.stat().st_size
            time.sleep(0.08)
            self.assertEqual(marker.stat().st_size, size)


if __name__ == "__main__": unittest.main()
