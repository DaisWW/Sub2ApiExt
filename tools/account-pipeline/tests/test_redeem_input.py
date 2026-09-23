"""卡密输入解析回归测试。"""

from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from redeem import main as redeem


class RedeemInputTests(unittest.TestCase):
    def test_read_codes_ignores_comments_before_and_after_codes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "redeem-codes.txt"
            path.write_text(
                "  # 已停用的卡密\n"
                "PLUS-AAA-111111 旧账号备注\n"
                "\tPLUS-BBB-222222\t# 新批次\n"
                "\n",
                encoding="utf-8",
            )

            self.assertEqual(
                redeem.read_codes(path), ["PLUS-AAA-111111", "PLUS-BBB-222222"]
            )


if __name__ == "__main__":
    unittest.main()
