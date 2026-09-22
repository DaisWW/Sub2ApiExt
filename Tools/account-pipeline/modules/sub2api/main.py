"""调用现有 Sub2API PowerShell 导入器。"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Optional


IMPORTER = (
    Path(__file__).resolve().parents[3]
    / "codex-account-import"
    / "import-codex-accounts.ps1"
)


class Sub2ApiModuleError(RuntimeError):
    """Sub2API 模块配置错误。"""


def powershell_executable() -> str:
    for name in ("powershell.exe", "pwsh.exe", "pwsh"):
        found = shutil.which(name)
        if found:
            return found
    raise Sub2ApiModuleError("找不到 PowerShell；请安装 Windows PowerShell 5.1 或 PowerShell 7")


def validate(input_file: Path, config_file: Optional[Path] = None) -> None:
    if not IMPORTER.is_file():
        raise Sub2ApiModuleError(f"找不到 Sub2API 导入脚本：{IMPORTER}")
    if not input_file.is_file():
        raise Sub2ApiModuleError(f"找不到 Sub2API 输入文件：{input_file}")
    if config_file is not None and not config_file.is_file():
        raise Sub2ApiModuleError(f"找不到 Sub2API 配置文件：{config_file}")


def execute(
    input_file: Path,
    config_file: Optional[Path] = None,
    *,
    what_if: bool = False,
) -> int:
    input_file = input_file.expanduser().resolve()
    config_file = config_file.expanduser().resolve() if config_file is not None else None
    validate(input_file, config_file)
    command = [
        powershell_executable(),
        "-NoProfile",
        "-ExecutionPolicy",
        "Bypass",
        "-File",
        str(IMPORTER),
        "-InputPath",
        str(input_file),
    ]
    if config_file is not None:
        command.extend(("-ConfigPath", str(config_file)))
    if what_if:
        command.append("-WhatIf")
    completed = subprocess.run(command, cwd=str(IMPORTER.parent), check=False)
    return int(completed.returncode)


def main() -> int:
    parser = argparse.ArgumentParser(description="运行 Sub2API 导入模块")
    parser.add_argument("input_file", type=Path)
    parser.add_argument("--config", type=Path)
    parser.add_argument("--what-if", action="store_true")
    args = parser.parse_args()
    try:
        return execute(args.input_file, args.config, what_if=args.what_if)
    except (OSError, Sub2ApiModuleError) as exc:
        print(f"Sub2API 模块失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
