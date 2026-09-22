"""调用 Cockpit Tools 本地导入桥接脚本。"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
from pathlib import Path


BRIDGE = Path(__file__).resolve().parent / "bridge.ps1"


class CockpitModuleError(RuntimeError):
    """Cockpit 模块配置错误。"""


def powershell_executable() -> str:
    for name in ("powershell.exe", "pwsh.exe", "pwsh"):
        found = shutil.which(name)
        if found:
            return found
    raise CockpitModuleError("找不到 PowerShell；请安装 Windows PowerShell 5.1 或 PowerShell 7")


def validate(input_file: Path) -> None:
    if not BRIDGE.is_file():
        raise CockpitModuleError(f"找不到 Cockpit 导入桥接脚本：{BRIDGE}")
    if not input_file.is_file():
        raise CockpitModuleError(f"找不到 Cockpit 输入文件：{input_file}")


def execute(input_file: Path, *, wait_seconds: int = 60) -> int:
    input_file = input_file.expanduser().resolve()
    validate(input_file)
    command = [
        powershell_executable(),
        "-NoProfile",
        "-ExecutionPolicy",
        "Bypass",
        "-File",
        str(BRIDGE),
        "-InputPath",
        str(input_file),
        "-WaitSeconds",
        str(wait_seconds),
    ]
    completed = subprocess.run(command, cwd=str(BRIDGE.parent), check=False)
    return int(completed.returncode)


def main() -> int:
    parser = argparse.ArgumentParser(description="运行 Cockpit Tools 导入模块")
    parser.add_argument("input_file", type=Path)
    parser.add_argument("--wait-seconds", type=int, default=60)
    args = parser.parse_args()
    try:
        return execute(args.input_file, wait_seconds=args.wait_seconds)
    except (OSError, CockpitModuleError) as exc:
        print(f"Cockpit 模块失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
