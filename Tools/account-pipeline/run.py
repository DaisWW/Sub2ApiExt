"""账号完整流水线：合并卡密兑换结果和账户文本后双目标导入。"""

from __future__ import annotations

import argparse
from contextlib import contextmanager
import json
import os
import sys
import uuid
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, Optional


BASE_DIR = Path(__file__).resolve().parent
if str(BASE_DIR) not in sys.path:
    sys.path.insert(0, str(BASE_DIR))

from cockpit import main as cockpit_module  # noqa: E402
from normalize import main as normalize_module  # noqa: E402
from redeem import main as redeem_module  # noqa: E402
from sub2api import main as sub2api_module  # noqa: E402


NEW_CODES_FILE = BASE_DIR / "input" / "redeem-codes.txt"
NEW_ACCOUNTS_FILE = BASE_DIR / "input" / "accounts.txt"
DEFAULT_RUNTIME_DIR = BASE_DIR / "cache"
PIPELINE_CONFIG = BASE_DIR / "config.json"


class PipelineError(RuntimeError):
    """流水线输入或运行状态错误。"""


class TeeStream:
    """同时保留控制台输出和本次运行日志。"""

    def __init__(self, console, log_file):
        self.console = console
        self.log_file = log_file

    def write(self, value: str) -> int:
        self.console.write(value)
        self.log_file.write(value)
        return len(value)

    def flush(self) -> None:
        self.console.flush()
        self.log_file.flush()

    def isatty(self) -> bool:
        return self.console.isatty()


@contextmanager
def capture_log(path: Path):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8", errors="replace", buffering=1) as log_file:
        original_stdout, original_stderr = sys.stdout, sys.stderr
        sys.stdout = TeeStream(original_stdout, log_file)
        sys.stderr = TeeStream(original_stderr, log_file)
        try:
            yield
        finally:
            sys.stdout = original_stdout
            sys.stderr = original_stderr


def resolved(path: Path) -> Path:
    return path.expanduser().resolve()


def config_path(value: Any) -> Optional[Path]:
    if not isinstance(value, str) or not value.strip():
        return None
    path = Path(value).expanduser()
    if not path.is_absolute():
        path = BASE_DIR / path
    return resolved(path)


def load_pipeline_config() -> Dict[str, Any]:
    if not PIPELINE_CONFIG.is_file():
        return {}
    try:
        value = json.loads(PIPELINE_CONFIG.read_text(encoding="utf-8-sig"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PipelineError(f"流水线配置文件无效：{PIPELINE_CONFIG}") from exc
    if not isinstance(value, dict):
        raise PipelineError(f"流水线配置必须是 JSON 对象：{PIPELINE_CONFIG}")
    return value


def runtime_dir_for_args(args: argparse.Namespace) -> Path:
    if args.runtime_dir is not None:
        return resolved(args.runtime_dir)
    try:
        config = load_pipeline_config()
    except PipelineError:
        return resolved(DEFAULT_RUNTIME_DIR)
    return config_path(config.get("runtime_dir")) or resolved(DEFAULT_RUNTIME_DIR)


def looks_like_account_text(path: Path) -> bool:
    """识别拖入总入口的账户 JSON 文本；卡密仍按普通文本处理。"""
    if path.suffix.lower() in {".json", ".jsonl"}:
        return True
    try:
        sample = path.read_bytes()[:4096].decode("utf-8-sig")
    except (OSError, UnicodeDecodeError):
        return False
    return sample.lstrip().startswith(("{", "["))


def choose_inputs(
    argument: Optional[Path],
    codes_argument: Optional[Path],
    accounts_argument: Optional[Path],
) -> tuple[Optional[Path], Optional[Path]]:
    if argument is not None and codes_argument is not None:
        raise PipelineError("位置参数不能与 --codes-file 同时使用")

    codes_file = resolved(codes_argument) if codes_argument is not None else None
    accounts_file = (
        resolved(accounts_argument) if accounts_argument is not None else None
    )

    if argument is not None:
        path = resolved(argument)
        if not path.is_file():
            raise PipelineError(f"找不到拖入的输入文件：{path}")
        if accounts_argument is not None:
            codes_file = path
        elif looks_like_account_text(path):
            accounts_file = path
        else:
            codes_file = path

    if argument is None and codes_argument is None and accounts_argument is None:
        if codes_file is None and NEW_CODES_FILE.is_file():
            codes_file = resolved(NEW_CODES_FILE)
        if accounts_file is None and NEW_ACCOUNTS_FILE.is_file():
            accounts_file = resolved(NEW_ACCOUNTS_FILE)
    if codes_file is None and accounts_file is None:
        raise PipelineError(
            "没有找到输入；请填写 input\\redeem-codes.txt 或 input\\accounts.txt，"
            "也可以把卡密/账户文本拖到 run.bat"
        )
    return codes_file, accounts_file


def make_run_dir(runtime_dir: Path) -> Path:
    run_id = datetime.now().strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:8]
    return runtime_dir / "runs" / run_id


def write_json(path: Path, value: Dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    try:
        temporary.write_text(
            json.dumps(value, ensure_ascii=False, indent=2) + "\n",
            encoding="utf-8",
        )
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise PipelineError(f"写入流水线 manifest 失败：{path}") from exc


def validate_layout(
    codes_file: Optional[Path],
    accounts_file: Optional[Path],
    sub2api_config: Optional[Path],
) -> None:
    if sys.version_info < (3, 8):
        raise PipelineError("需要 Python 3.8 或更高版本")
    if codes_file is not None:
        redeem_module.validate(codes_file)
    if accounts_file is not None:
        normalize_module.validate_input_file(accounts_file)
    if sub2api_config is not None:
        sub2api_module.validate_config(sub2api_config)


def read_redeem_manifest(path: Path, fallback_data_dir: Path) -> Dict[str, Any]:
    if not path.is_file():
        data_dir = fallback_data_dir / "data"
        if data_dir.is_dir():
            return {"data_dir": str(data_dir.resolve()), "total": None}
        raise PipelineError("兑换完成但没有找到兑换 manifest 或解压目录")
    try:
        value = json.loads(path.read_text(encoding="utf-8-sig"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PipelineError("兑换 manifest 无法读取") from exc
    if not isinstance(value, dict):
        raise PipelineError("兑换 manifest 格式无效")
    data_dir_value = value.get("data_dir")
    if isinstance(data_dir_value, str) and data_dir_value.strip():
        data_dir_candidate = Path(data_dir_value).expanduser()
        if not data_dir_candidate.is_absolute():
            data_dir_candidate = path.parent / data_dir_candidate
        data_dir = resolved(data_dir_candidate)
    else:
        data_dir = resolved(fallback_data_dir / "data")
    if not data_dir.is_dir():
        raise PipelineError(f"找不到兑换解压目录：{data_dir}")
    value["data_dir"] = str(data_dir)
    return value


def stage_error(
    manifest: Dict[str, Any],
    path: Path,
    stage: str,
    code: int,
    message: str,
) -> int:
    manifest.update(
        {
            "status": "failed",
            "failed_stage": stage,
            "exit_code": code,
            "error": message,
            "finished_at": datetime.now().isoformat(timespec="seconds"),
        }
    )
    write_json(path, manifest)
    print(f"流水线在 {stage} 阶段失败：{message}", file=sys.stderr)
    return code if code > 0 else 1


def run(args: argparse.Namespace) -> int:
    config = load_pipeline_config()
    codes_file, accounts_file = choose_inputs(
        args.input_file, args.codes_file, args.accounts_file
    )

    if args.runtime_dir is not None:
        runtime_dir = resolved(args.runtime_dir)
    else:
        runtime_dir = config_path(config.get("runtime_dir")) or resolved(
            DEFAULT_RUNTIME_DIR
        )

    sub2api_config = (
        resolved(args.sub2api_config)
        if args.sub2api_config is not None
        else config_path(config.get("sub2api_config"))
    )
    wait_seconds = args.wait_seconds
    if wait_seconds is None and "cockpit_wait_seconds" in config:
        try:
            wait_seconds = int(config["cockpit_wait_seconds"])
        except (TypeError, ValueError) as exc:
            raise PipelineError("cockpit_wait_seconds 必须是整数") from exc
    if wait_seconds is None:
        wait_seconds = 60
    if not 10 <= wait_seconds <= 300:
        raise PipelineError("Cockpit 等待时间必须在 10 到 300 秒之间")

    validate_layout(codes_file, accounts_file, sub2api_config)
    if codes_file is not None:
        print(f"卡密输入：{codes_file}")
    else:
        print("卡密输入：未提供（跳过兑换）")
    if accounts_file is not None:
        print(f"账户文本输入：{accounts_file}")
    else:
        print("账户文本输入：未提供")
    print(f"运行根目录：{runtime_dir}")
    if args.dry_run:
        print("检查通过；dry-run 未访问兑换站，也未执行导入。")
        return 0

    run_dir = make_run_dir(runtime_dir)
    redeem_dir = run_dir / "redeem"
    normalized_dir = run_dir / "normalized"
    result_file = runtime_dir / "results" / "redeem-result.txt"
    redeem_manifest = run_dir / "redeem-manifest.json"
    pipeline_manifest = run_dir / "manifest.json"
    manifest: Dict[str, Any] = {
        "version": 1,
        "status": "running",
        "started_at": datetime.now().isoformat(timespec="seconds"),
        "input_file": str(codes_file or accounts_file),
        "codes_file": str(codes_file) if codes_file is not None else None,
        "accounts_file": str(accounts_file) if accounts_file is not None else None,
        "run_dir": str(run_dir),
        "result_file": str(result_file) if codes_file is not None else None,
        "redeem_manifest": str(redeem_manifest) if codes_file is not None else None,
        "normalized_dir": str(normalized_dir),
        "skip_sub2api": bool(args.skip_sub2api),
        "skip_cockpit": bool(args.skip_cockpit),
    }
    stage = "redeem" if codes_file is not None else "normalize"
    try:
        write_json(pipeline_manifest, manifest)
        redeem_code = 0
        data_dir: Optional[Path] = None
        if codes_file is not None:
            redeem_code = redeem_module.execute(
                codes_file,
                action="sub2api",
                cache_dir=runtime_dir / "runs",
                run_dir=redeem_dir,
                result_file=result_file,
                manifest_file=redeem_manifest,
            )
            manifest["redeem_exit_code"] = redeem_code
            manifest["redeem_result_file"] = str(result_file)
            if redeem_code not in (0, 2):
                return stage_error(manifest, pipeline_manifest, "redeem", redeem_code, "兑换未完成")

            redeem_info = read_redeem_manifest(redeem_manifest, redeem_dir)
            data_dir = Path(redeem_info["data_dir"])
            manifest["redeem"] = {
                key: redeem_info.get(key)
                for key in ("task_id", "archive", "data_dir", "total", "success", "failed")
                if key in redeem_info
            }
            if redeem_code == 2:
                print("兑换结果包含失败卡密；将继续处理已下载的成功账号。")
        else:
            print("未提供卡密，跳过兑换阶段。")

        stage = "normalize"
        print("[流水线] 开始标准化账号数据……")
        normalized = normalize_module.normalize(
            data_dir, normalized_dir, accounts_file
        )
        manifest["normalized"] = normalized
        write_json(pipeline_manifest, manifest)
        print(f"标准化完成：{normalized['accounts']} 个账号")

        if not args.skip_sub2api:
            stage = "sub2api"
            print("[流水线] 开始导入 Sub2API……")
            sub2api_code = sub2api_module.execute(
                Path(normalized["sub2api_input"]), sub2api_config
            )
            manifest["sub2api_exit_code"] = sub2api_code
            write_json(pipeline_manifest, manifest)
            if sub2api_code != 0:
                return stage_error(
                    manifest, pipeline_manifest, "sub2api", sub2api_code, "Sub2API 导入未完成"
                )
        else:
            print("已跳过 Sub2API 导入。")

        if not args.skip_cockpit:
            stage = "cockpit"
            print("[流水线] 开始导入 Cockpit（可能需要较长时间）……")
            cockpit_code = cockpit_module.execute(
                Path(normalized["cockpit_input"]), wait_seconds=wait_seconds
            )
            manifest["cockpit_exit_code"] = cockpit_code
            write_json(pipeline_manifest, manifest)
            if cockpit_code != 0:
                return stage_error(
                    manifest, pipeline_manifest, "cockpit", cockpit_code, "Cockpit 导入未完成"
                )
        else:
            print("已跳过 Cockpit 导入。")

        final_code = 2 if redeem_code == 2 else 0
        manifest.update(
            {
                "status": "partial" if final_code == 2 else "success",
                "finished_at": datetime.now().isoformat(timespec="seconds"),
                "exit_code": final_code,
            }
        )
        write_json(pipeline_manifest, manifest)
        if final_code == 2:
            print(f"流水线完成，但有卡密失败；运行记录：{pipeline_manifest}")
        else:
            print(f"流水线完成。运行记录：{pipeline_manifest}")
        return final_code
    except (
        OSError,
        ValueError,
        TypeError,
        KeyError,
        normalize_module.NormalizeError,
        redeem_module.RedeemError,
        sub2api_module.Sub2ApiError,
        cockpit_module.CockpitError,
        PipelineError,
    ) as exc:
        return stage_error(manifest, pipeline_manifest, stage, 1, str(exc))


def main() -> int:
    parser = argparse.ArgumentParser(
        description="处理卡密和账户 JSON 文本，标准化后导入 Sub2API 和 Cockpit"
    )
    parser.add_argument(
        "input_file",
        nargs="?",
        type=Path,
        help="拖入的卡密或账户 JSON 文本；也可分别使用下面两个选项",
    )
    parser.add_argument("--codes-file", type=Path, help="卡密 TXT（一行一个，允许空行）")
    parser.add_argument(
        "--accounts-file",
        type=Path,
        help="账户 JSON 文本（可连续放多个对象，允许空行）",
    )
    parser.add_argument("--runtime-dir", type=Path, help="覆盖默认的 account-pipeline 缓存目录")
    parser.add_argument("--sub2api-config", type=Path, help="Sub2API 导入配置 JSON")
    parser.add_argument("--wait-seconds", type=int, help="Cockpit 导入等待秒数")
    parser.add_argument("--skip-sub2api", action="store_true", help="跳过 Sub2API 导入")
    parser.add_argument("--skip-cockpit", action="store_true", help="跳过 Cockpit 导入")
    parser.add_argument("--log-file", type=Path, help="覆盖本次流水线日志路径")
    parser.add_argument("--dry-run", action="store_true", help="只检查本地文件，不访问网络")
    args = parser.parse_args()
    try:
        runtime_dir = runtime_dir_for_args(args)
        log_file = (
            resolved(args.log_file)
            if args.log_file is not None
            else runtime_dir
            / "logs"
            / f"pipeline-{datetime.now().strftime('%Y%m%d-%H%M%S')}-{uuid.uuid4().hex[:8]}.log"
        )
        with capture_log(log_file):
            print(f"[日志] 本次流水线日志：{log_file}")
            return run(args)
    except (
        PipelineError,
        OSError,
        ValueError,
        TypeError,
        KeyError,
        normalize_module.NormalizeError,
        redeem_module.RedeemError,
        sub2api_module.Sub2ApiError,
    ) as exc:
        print(f"流水线失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
