"""公共预检查和运行锁的离线回归测试；所有输出使用临时目录。"""

from __future__ import annotations

import contextlib
import io
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


BASE_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(BASE_DIR))

import run as pipeline


MODES = ((), ("--incremental",), ("--refresh-tokens",))


class PipelineCommonTests(unittest.TestCase):
    def run_check(self, root: Path, accounts: Path, mode: tuple[str, ...], codes=None):
        log = root / ("check-" + (mode[0].lstrip("-") if mode else "full") + ".log")
        argv = [
            "run.py", "--accounts-file", str(accounts), "--runtime-dir", str(root / "cache"),
            "--log-file", str(log), "--dry-run", *mode,
        ]
        if codes is not None:
            argv.extend(("--codes-file", str(codes)))
        console = io.StringIO()
        with patch.object(sys, "argv", argv), patch.object(
            pipeline, "load_pipeline_config", return_value={}
        ), contextlib.redirect_stdout(console), contextlib.redirect_stderr(console):
            code = pipeline.main()
        return code, console.getvalue(), log.read_text(encoding="utf-8")

    def test_empty_codes_pass_the_same_precheck_in_all_modes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text(json.dumps({
                "type": "codex", "email": "one@example.com", "access_token": "placeholder",
            }), encoding="utf-8")
            codes = root / "codes.txt"
            for content in ("", "\n \t\n", "# DISABLED\n"):
                codes.write_text(content, encoding="utf-8")
                for mode in MODES:
                    with self.subTest(mode=mode, content=repr(content)):
                        code, _, log = self.run_check(root, accounts, mode, codes)
                        self.assertEqual(code, 0)
                        self.assertIn("检查通过", log)
            self.assertFalse((root / "cache" / "state").exists())

    def test_local_conflicts_fail_the_same_precheck_in_all_modes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text(json.dumps([
                {"type": "codex", "email": "same@example.com", "access_token": token}
                for token in ("placeholder-one", "placeholder-two")
            ]), encoding="utf-8")
            for mode in MODES:
                with self.subTest(mode=mode):
                    code, console, log = self.run_check(root, accounts, mode)
                    self.assertEqual(code, 1)
                    self.assertIn("冲突", console)
                    self.assertIn("冲突", log)
                    report = root / "cache" / "results" / "input-conflicts.txt"
                    self.assertTrue(report.is_file())
                    for token in ("placeholder-one", "placeholder-two"):
                        self.assertNotIn(token, log)
                        self.assertNotIn(token, report.read_text(encoding="utf-8-sig"))

    def test_precheck_errors_are_logged_in_all_modes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text("{", encoding="utf-8")
            for mode in MODES:
                with self.subTest(mode=mode):
                    code, console, log = self.run_check(root, accounts, mode)
                    self.assertEqual(code, 1)
                    self.assertIn("流水线失败", console)
                    self.assertIn("流水线失败", log)

    def test_runtime_lock_blocks_other_modes_and_releases_after_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text(json.dumps({
                "type": "codex", "email": "one@example.com", "access_token": "placeholder",
            }), encoding="utf-8")
            runtime = root / "cache"
            for mode in MODES:
                with self.subTest(mode=mode):
                    argv = [
                        "run.py", "--accounts-file", str(accounts), "--runtime-dir", str(runtime),
                        "--dry-run", *mode,
                    ]
                    code = (
                        "import sys; from pathlib import Path; "
                        f"sys.path.insert(0, {str(BASE_DIR)!r}); import run as pipeline; "
                        f"pipeline.PIPELINE_CONFIG = Path({str(root / 'missing-config.json')!r}); "
                        f"sys.argv = {argv!r}; raise SystemExit(pipeline.main())"
                    )
                    command = [sys.executable, "-B", "-X", "utf8", "-c", code]
                    with pipeline.runtime_lock(runtime):
                        blocked = subprocess.run(
                            command, capture_output=True, encoding="utf-8", timeout=10,
                        )
                    self.assertEqual(blocked.returncode, 1)
                    self.assertIn("运行目录", blocked.stderr)
                    completed = subprocess.run(
                        command, capture_output=True, encoding="utf-8", timeout=10,
                    )
                    self.assertEqual(completed.returncode, 0, completed.stderr)
            self.assertFalse((runtime / "state").exists())

    def test_runtime_lock_releases_after_error(self):
        with tempfile.TemporaryDirectory() as directory:
            runtime = Path(directory)
            with self.assertRaisesRegex(RuntimeError, "test failure"):
                with pipeline.runtime_lock(runtime):
                    raise RuntimeError("test failure")
            with pipeline.runtime_lock(runtime):
                pass


if __name__ == "__main__":
    unittest.main()
