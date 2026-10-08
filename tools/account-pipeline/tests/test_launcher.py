"""BAT 公共启动器回归测试；使用临时脚本验证参数和退出码。"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


BASE_DIR = Path(__file__).resolve().parents[1]


class LauncherTests(unittest.TestCase):
    @unittest.skipUnless(os.name == "nt", "BAT 入口只适用于 Windows")
    def test_bat_entries_preserve_arguments_modes_and_exit_codes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ("run.bat", "incremental.bat", "refresh-tokens.bat", "launch.py"):
                shutil.copyfile(BASE_DIR / name, root / name)
            result = root / "arguments.json"
            script = (
                "import json, sys; from pathlib import Path; "
                f"Path({str(result)!r}).write_text(json.dumps(sys.argv[1:]), encoding='utf-8'); "
                "raise SystemExit(7)\n"
            )
            (root / "run.py").write_text(script, encoding="utf-8")
            module = root / "module with spaces.py"
            module.write_text(script, encoding="utf-8")
            cases = [
                ("run.bat", [], []),
                ("incremental.bat", [], ["--incremental"]),
                ("refresh-tokens.bat", [], ["--refresh-tokens"]),
                ("run.bat", ["--script", str(module)], []),
            ]
            arguments = ["--marker", "value with spaces", "x & y", ""]
            for entry, prefix, mode in cases:
                with self.subTest(entry=entry, prefix=prefix):
                    process = subprocess.run(
                        ["cmd.exe", "/d", "/c", str(root / entry), *prefix, *arguments],
                        input="\n", capture_output=True, encoding="utf-8", errors="replace", timeout=10,
                        env={**os.environ, "PYTHONUTF8": "1", "PYTHONDONTWRITEBYTECODE": "1"},
                    )
                    self.assertEqual(process.returncode, 7, process.stderr)
                    self.assertEqual(json.loads(result.read_text(encoding="utf-8")), [*mode, *arguments])


if __name__ == "__main__":
    unittest.main()
