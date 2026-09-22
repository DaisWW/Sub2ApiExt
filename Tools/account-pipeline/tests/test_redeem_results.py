"""兑换结果表的对齐回归测试。"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from redeem import main as redeem


class RedeemResultTests(unittest.TestCase):
    def test_description_stays_in_its_column(self):
        message = "账号已被官方封禁；请联系支持并重新下载 JSON 文件。" * 2
        rendered = redeem.format_results(
            [
                {
                    "code": "PLUS-ABC-123456",
                    "account": "very-long-account-name@example.com",
                    "extracted_at": "2026-09-22 18:33:11",
                    "warranty_left": "已过期",
                    "ok": False,
                    "state": "banned",
                    "message": message,
                }
            ],
            terminal_columns=120,
        )
        lines = rendered.splitlines()
        self.assertIn("说明", lines[1])
        self.assertNotIn("说明：", rendered)
        table_lines = lines[1:-1]
        self.assertGreaterEqual(len(table_lines), 4)
        self.assertEqual(
            {redeem.display_width(line) for line in table_lines},
            {redeem.display_width(table_lines[0])},
        )
        data_lines = table_lines[2:]
        self.assertIn("…", data_lines[0].split(" | ")[2])
        self.assertTrue(
            all(
                not any(part.strip() for part in line.split(" | ")[:6])
                for line in data_lines[1:]
            )
        )
        description_parts = [line.split(" | ")[-1].rstrip() for line in data_lines]
        self.assertEqual(
            "".join(description_parts).replace(" ", ""),
            message.replace(" ", ""),
        )
        for line in table_lines:
            if "-+-" in line:
                self.assertEqual(line.count("-+-"), 6)
            else:
                self.assertEqual(line.count(" | "), 6)


if __name__ == "__main__":
    unittest.main()
