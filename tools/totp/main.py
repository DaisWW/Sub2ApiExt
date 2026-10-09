"""从本地输入文件生成六位 TOTP 验证码，不访问网络。"""

from __future__ import annotations

import argparse
import base64
import binascii
import hmac
import sys
import time
from pathlib import Path
from typing import List, Optional, Tuple


class TotpGenerator:
    """实现 RFC 6238 的 SHA-1、六位数字、30 秒时间窗口。"""

    PERIOD = 30
    DIGITS = 6

    def __init__(self, secret: str):
        normalized = "".join(secret.split()).upper()
        if not normalized:
            raise ValueError("2FA 密钥不能为空")
        try:
            self._key = base64.b32decode(normalized + "=" * (-len(normalized) % 8))
        except (binascii.Error, ValueError):
            raise ValueError("2FA 密钥必须是有效的 Base32 文本") from None

    def at(self, timestamp: float) -> Tuple[str, int]:
        seconds = int(timestamp)
        counter = seconds // self.PERIOD
        digest = hmac.digest(self._key, counter.to_bytes(8, "big"), "sha1")
        offset = digest[-1] & 0x0F
        number = int.from_bytes(digest[offset:offset + 4], "big") & 0x7FFFFFFF
        code = str(number % (10 ** self.DIGITS)).zfill(self.DIGITS)
        return code, self.PERIOD - seconds % self.PERIOD


class TotpConsole:
    """读取输入并输出验证码；单行错误不会阻塞其他有效输入。"""

    def __init__(self, input_file: Path):
        self.input_file = input_file

    @staticmethod
    def parse_entry(line: str, line_number: int) -> Tuple[str, TotpGenerator]:
        fields = line.split("----")
        if len(fields) == 1:
            label, secret = f"第 {line_number} 行", fields[0]
        elif len(fields) == 3:
            label, secret = fields[0].strip(), fields[2]
            address = label.split("@")
            if len(address) != 2 or not all(address) or any(c.isspace() for c in label):
                raise ValueError("账户格式第一项必须是有效邮箱")
        else:
            raise ValueError("请使用纯密钥或邮箱----密码----2FA密钥格式")
        return label, TotpGenerator(secret)

    def run(self) -> int:
        try:
            text = self.input_file.read_text(encoding="utf-8-sig")
        except FileNotFoundError:
            print(
                f"未找到输入文件：{self.input_file}\n"
                "请将 input/secrets.example.txt 复制为 input/secrets.txt，再填写自己的密钥。",
                file=sys.stderr,
            )
            return 2
        except (OSError, UnicodeError):
            print("无法读取输入文件，请检查文件权限和 UTF-8 编码。", file=sys.stderr)
            return 2

        succeeded = failed = 0
        for line_number, line in enumerate(text.splitlines(), 1):
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            try:
                label, generator = self.parse_entry(line, line_number)
                code, remaining = generator.at(time.time())
            except ValueError as exc:
                print(f"第 {line_number} 行错误：{exc}", file=sys.stderr)
                failed += 1
                continue
            print(f"{label}\t验证码：{code}\t剩余有效期约 {remaining} 秒")
            succeeded += 1

        if failed:
            print(f"已生成 {succeeded} 条验证码，{failed} 行输入无效。", file=sys.stderr)
            return 1
        if not succeeded:
            print("输入文件没有有效内容，请填写至少一行 2FA 密钥。", file=sys.stderr)
            return 2
        return 0


def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(description="离线生成六位 TOTP 验证码（SHA-1 / 30 秒）。")
    parser.add_argument(
        "input_file",
        nargs="?",
        type=Path,
        default=Path(__file__).resolve().parent / "input" / "secrets.txt",
        help="UTF-8 输入文件；默认读取本工具的 input/secrets.txt",
    )
    args = parser.parse_args(argv)
    return TotpConsole(args.input_file).run()


if __name__ == "__main__":
    raise SystemExit(main())
