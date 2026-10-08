"""兑换码邮箱映射的状态、累计保存和中断回归测试。"""

import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from inputs import InputFiles
from redeem import main as redeem


class RedeemMappingTests(unittest.TestCase):
    def test_records_every_status_and_email_field(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "map.json"
            rows = [
                {"code": "NORMAL", "account": "OK@EXAMPLE.COM", "ok": True},
                {"code": "BANNED", "account": r"banned\@example.com", "ok": False, "state": "banned"},
                {"code": "EXPIRED", "email": "expired@example.com", "ok": False, "state": "expired"},
                {"code": "QUEUED", "user_email": "queued@example.com", "state": "queued"},
                {"code": "UNKNOWN", "account_email": "unknown@example.com", "state": "unknown"},
                {"code": "NO-EMAIL", "account": "未获取到"},
                {"account": "no-code@example.com"},
                {"code": "NORMAL", "account": "ok@example.com", "email": "other@example.com"},
            ]
            redeem.update_code_email_map(path, rows)
            mapping = json.loads(path.read_text(encoding="utf-8"))
            self.assertEqual(mapping, {
                "NORMAL": ["ok@example.com", "other@example.com"],
                "BANNED": ["banned@example.com"], "EXPIRED": ["expired@example.com"],
                "QUEUED": ["queued@example.com"], "UNKNOWN": ["unknown@example.com"], "NO-EMAIL": [],
            })

    def test_retains_previous_emails_when_later_result_fails_or_is_empty(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "map.json"
            redeem.update_code_email_map(path, [{"code": "CARD", "account": "old@example.com", "ok": True}])
            redeem.update_code_email_map(path, [{"code": "CARD", "ok": False}])
            redeem.update_code_email_map(path, [{"code": "CARD", "email": "new@example.com", "ok": False}])
            redeem.update_code_email_map(path, [])
            self.assertEqual(json.loads(path.read_text(encoding="utf-8")), {"CARD": ["new@example.com", "old@example.com"]})
            self.assertFalse(path.with_name("map.json.tmp").exists())

    def test_invalid_existing_file_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "map.json"
            for text in ("invalid", '[]', '{"PRIVATE-CARD":"old@example.com"}'):
                path.write_text(text, encoding="utf-8")
                with self.assertRaises(redeem.RedeemError) as error:
                    redeem.update_code_email_map(path, [{"code": "NEW", "account": "new@example.com"}])
                self.assertNotIn("PRIVATE-CARD", str(error.exception))
                self.assertEqual(path.read_text(encoding="utf-8"), text)

    def test_received_progress_survives_connection_interruption(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "map.json"
            payload = {"done": 1, "total": 2, "code": "CARD", "account": "partial@example.com", "ok": False}
            events = ("event: progress\ndata: " + json.dumps(payload) + "\n\n").encode("utf-8")
            client = redeem.RedeemClient()
            with patch.object(redeem, "CODE_EMAIL_MAP_FILE", path), patch.object(
                client, "request", return_value=io.BytesIO(events)
            ), contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaisesRegex(redeem.RedeemError, "任务连接提前结束"):
                    client.wait("test-task-id")
            self.assertEqual(json.loads(path.read_text(encoding="utf-8")), {"CARD": ["partial@example.com"]})

    def test_result_mapping_survives_download_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codes = root / "codes.txt"
            codes.write_text("CARD\n", encoding="utf-8")
            mapping = root / "input" / "redeem-code-email-map.json"
            client = MagicMock()
            client.submit.return_value = "test-task-id"
            client.wait.return_value = {"rows": [{"code": "CARD", "account": "banned@example.com", "ok": False, "state": "banned"}]}
            client.download.side_effect = redeem.RedeemError("下载失败")
            with patch.object(redeem, "RedeemClient", return_value=client), patch.object(
                redeem, "CODE_EMAIL_MAP_FILE", mapping
            ), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                self.assertEqual(redeem.execute(codes, cache_dir=root / "cache", result_file=root / "result.txt"), 1)
            self.assertEqual(json.loads(mapping.read_text(encoding="utf-8")), {"CARD": ["banned@example.com"]})

    def test_generated_mapping_is_excluded_from_input_discovery(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            redeem.update_code_email_map(root / "redeem-code-email-map.json", [{"code": "CARD", "email": "one@example.com"}])
            inputs = InputFiles.discover(root)
            self.assertEqual(inputs.accounts_files, [])
            self.assertEqual(inputs.codes_files, [])


if __name__ == "__main__":
    unittest.main()
