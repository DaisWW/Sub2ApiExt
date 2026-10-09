"""验证标准 TOTP 向量、输入边界和控制台凭据保护。"""

from __future__ import annotations

import importlib.util
import io
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock


spec = importlib.util.spec_from_file_location("totp_tool", Path(__file__).resolve().parents[1] / "main.py")
totp = importlib.util.module_from_spec(spec)
spec.loader.exec_module(totp)

PUBLIC_SECRET = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"


class TotpGeneratorTests(unittest.TestCase):
    def test_rfc6238_sha1_vectors_with_six_digits(self):
        generator = totp.TotpGenerator(PUBLIC_SECRET)
        # RFC 6238 Appendix B uses eight digits; six-digit output is its last six digits.
        for timestamp, expected in (
            (59, "287082"),
            (1111111109, "081804"),
            (1111111111, "050471"),
            (1234567890, "005924"),
            (2000000000, "279037"),
            (20000000000, "353130"),
        ):
            with self.subTest(timestamp=timestamp):
                self.assertEqual(generator.at(timestamp)[0], expected)

    def test_window_boundary_and_fractional_time(self):
        generator = totp.TotpGenerator(PUBLIC_SECRET)
        self.assertEqual(generator.at(59.9), ("287082", 1))
        self.assertEqual(generator.at(60), ("359152", 30))

    def test_normalizes_case_and_whitespace(self):
        generator = totp.TotpGenerator("gez dgnbv\tgy3tqojq gezdgnbvgy3tqojq\n")
        self.assertEqual(generator.at(59)[0], "287082")

    def test_invalid_secret_is_not_echoed(self):
        for secret in ("", "INVALID-SECRET!", "A", "123456"):
            with self.subTest(secret=secret):
                with self.assertRaises(ValueError) as caught:
                    totp.TotpGenerator(secret)
                expected = "2FA 密钥必须是有效的 Base32 文本" if secret else "2FA 密钥不能为空"
                self.assertEqual(str(caught.exception), expected)


class TotpConsoleTests(unittest.TestCase):
    def run_console(self, path):
        stdout, stderr = io.StringIO(), io.StringIO()
        with redirect_stdout(stdout), redirect_stderr(stderr), mock.patch.object(totp.time, "time", return_value=59):
            result = totp.TotpConsole(path).run()
        return result, stdout.getvalue(), stderr.getvalue()

    def test_plain_and_account_input_with_bom(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "secrets.txt"
            path.write_text(
                f"# public fixtures\n\n{PUBLIC_SECRET}\n"
                f"user@example.com----unused-password----{PUBLIC_SECRET}\n",
                encoding="utf-8-sig",
            )
            result, stdout, stderr = self.run_console(path)
        self.assertEqual(result, 0)
        self.assertEqual(stdout.count("287082"), 2)
        self.assertIn("user@example.com", stdout)
        self.assertIn("1 秒", stdout)
        self.assertEqual(stderr, "")
        self.assertNotIn(PUBLIC_SECRET, stdout)
        self.assertNotIn("unused-password", stdout)

    def test_invalid_lines_do_not_block_valid_input_or_echo_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "secrets.txt"
            path.write_text(
                f"invalid-secret\n{PUBLIC_SECRET}\n"
                "user@example.com----unused-password\n"
                "user@example.com----unused-password----invalid-account-secret\n",
                encoding="utf-8",
            )
            result, stdout, stderr = self.run_console(path)
        self.assertEqual(result, 1)
        self.assertEqual(stdout.count("287082"), 1)
        self.assertIn("第 1 行错误", stderr)
        self.assertIn("第 3 行错误", stderr)
        self.assertIn("第 4 行错误", stderr)
        for sensitive in (PUBLIC_SECRET, "invalid-secret", "invalid-account-secret", "unused-password"):
            self.assertNotIn(sensitive, stdout + stderr)

    def test_non_email_account_label_is_rejected_without_echoing_it(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "secrets.txt"
            path.write_text(f"fake-password-only----unused----{PUBLIC_SECRET}\n", encoding="utf-8")
            result, stdout, stderr = self.run_console(path)
        self.assertEqual(result, 1)
        self.assertEqual(stdout, "")
        self.assertIn("账户格式第一项必须是有效邮箱", stderr)
        self.assertNotIn("fake-password-only", stderr)

    def test_missing_empty_and_unreadable_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "secrets.txt"
            self.assertEqual(self.run_console(path)[0], 2)
            for content in (b"", b"# comments only\n", b"\xff"):
                with self.subTest(content=content):
                    path.write_bytes(content)
                    result, stdout, stderr = self.run_console(path)
                    self.assertEqual(result, 2)
                    self.assertEqual(stdout, "")
                    self.assertTrue(stderr)

    def test_default_input_is_relative_to_tool_directory(self):
        with mock.patch.object(totp, "TotpConsole") as console:
            console.return_value.run.return_value = 0
            self.assertEqual(totp.main([]), 0)
            console.assert_called_once_with(Path(totp.__file__).resolve().parent / "input" / "secrets.txt")


if __name__ == "__main__":
    unittest.main()
