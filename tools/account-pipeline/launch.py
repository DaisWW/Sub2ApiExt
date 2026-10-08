"""共用 BAT 启动入口；在 Python 中选择脚本并完整保留业务参数。"""

from __future__ import annotations

from pathlib import Path
import runpy
import sys


def main() -> int:
    arguments = sys.argv[1:]
    script = Path(__file__).resolve().with_name("run.py")
    if arguments and arguments[0].lower() == "--script":
        if len(arguments) < 2 or not arguments[1]:
            print("Python launcher requires a script path.", file=sys.stderr)
            return 2
        script = Path(arguments[1]).expanduser().resolve()
        arguments = arguments[2:]
    if not script.is_file():
        print(f"Python script was not found: {script}", file=sys.stderr)
        return 2
    sys.path.insert(0, str(script.parent))
    sys.argv = [str(script), *arguments]
    runpy.run_path(str(script), run_name="__main__")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
