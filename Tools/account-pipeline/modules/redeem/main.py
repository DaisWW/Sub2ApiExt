"""调用现有卡密兑换器，把结果放入本次 pipeline 运行目录。"""

from __future__ import annotations

import argparse
import subprocess
import sys
from pathlib import Path


LEGACY_SCRIPT = (
    Path(__file__).resolve().parents[3]
    / "codex-account-import"
    / "redeem-account-import.py"
)


class RedeemModuleError(RuntimeError):
    """兑换模块配置错误。"""


def validate(input_file: Path) -> None:
    """只检查本地输入，不访问兑换站。"""
    if not LEGACY_SCRIPT.is_file():
        raise RedeemModuleError(f"找不到兑换脚本：{LEGACY_SCRIPT}")
    if not input_file.is_file():
        raise RedeemModuleError(f"找不到卡密文件：{input_file}")
    if not input_file.read_bytes():
        raise RedeemModuleError(f"卡密文件为空：{input_file}")


def execute(
    input_file: Path,
    run_dir: Path,
    result_file: Path,
    manifest_file: Path,
) -> int:
    """运行旧兑换器并返回其退出码。"""
    input_file = input_file.expanduser().resolve()
    run_dir = run_dir.expanduser().resolve()
    result_file = result_file.expanduser().resolve()
    manifest_file = manifest_file.expanduser().resolve()
    validate(input_file)
    command = [
        sys.executable,
        str(LEGACY_SCRIPT),
        str(input_file),
        "--action",
        "sub2api",
        "--run-dir",
        str(run_dir),
        "--result-file",
        str(result_file),
        "--manifest-file",
        str(manifest_file),
    ]
    completed = subprocess.run(command, cwd=str(LEGACY_SCRIPT.parent), check=False)
    return int(completed.returncode)


def main() -> int:
    parser = argparse.ArgumentParser(description="运行账号流水线的卡密兑换模块")
    parser.add_argument("input_file", type=Path)
    parser.add_argument("--run-dir", required=True, type=Path)
    parser.add_argument("--result-file", required=True, type=Path)
    parser.add_argument("--manifest-file", required=True, type=Path)
    args = parser.parse_args()
    try:
        return execute(args.input_file, args.run_dir, args.result_file, args.manifest_file)
    except (OSError, RedeemModuleError) as exc:
        print(f"兑换模块失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
