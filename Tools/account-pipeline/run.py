"""账号完整流水线：兑换、标准化、Sub2API 导入、Cockpit 导入。"""

from __future__ import annotations

import argparse
import json
import os
import sys
import uuid
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, Optional

from modules.cockpit import main as cockpit_module
from modules.normalize import main as normalize_module
from modules.redeem import main as redeem_module
from modules.sub2api import main as sub2api_module


BASE_DIR = Path(__file__).resolve().parent
NEW_CODES_FILE = BASE_DIR / "input" / "redeem-codes.txt"
LEGACY_CODES_FILE = BASE_DIR.parent / "codex-account-import" / "redeem-codes.txt"
DEFAULT_RUNTIME_DIR = Path(
    os.environ.get("PROGRAMDATA") or r"C:\ProgramData"
) / "Sub2API" / "account-pipeline"
PIPELINE_CONFIG = BASE_DIR / "config.json"


class PipelineError(RuntimeError):
    """流水线输入或运行状态错误。"""


def resolved(path: Path) -> Path:
    return path.expanduser().resolve()


def config_path(value: Any) -> Optional[Path]:
    if not isinstance(value, str) or not value.strip():
        return None
    path = Path(value)
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


def choose_input(argument: Optional[Path]) -> Path:
    if argument is not None:
        path = resolved(argument)
        if not path.is_file():
            raise PipelineError(f"找不到拖入的卡密文件：{path}")
        return path
    for candidate in (NEW_CODES_FILE, LEGACY_CODES_FILE):
        if candidate.is_file():
            return resolved(candidate)
    raise PipelineError(
        "没有找到卡密文件；请把文件拖到 run.bat，或创建 "
        f"{NEW_CODES_FILE}（也兼容 {LEGACY_CODES_FILE}）"
    )


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


def validate_layout(input_file: Path, sub2api_config: Optional[Path]) -> None:
    if sys.version_info < (3, 8):
        raise PipelineError("需要 Python 3.8 或更高版本")
    if not input_file.is_file() or not input_file.read_bytes():
        raise PipelineError(f"卡密文件不存在或为空：{input_file}")
    redeem_module.validate(input_file)
    if not redeem_module.LEGACY_SCRIPT.is_file():
        raise PipelineError(f"找不到兑换脚本：{redeem_module.LEGACY_SCRIPT}")
    if not sub2api_module.IMPORTER.is_file():
        raise PipelineError(f"找不到 Sub2API 导入脚本：{sub2api_module.IMPORTER}")
    if not cockpit_module.BRIDGE.is_file():
        raise PipelineError(f"找不到 Cockpit 导入桥接脚本：{cockpit_module.BRIDGE}")
    if sub2api_config is not None and not sub2api_config.is_file():
        raise PipelineError(f"找不到 Sub2API 配置文件：{sub2api_config}")


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
    if isinstance(data_dir_value, str):
        data_dir_candidate = Path(data_dir_value)
        if not data_dir_candidate.is_absolute():
            data_dir_candidate = path.parent / data_dir_candidate
        data_dir = resolved(data_dir_candidate)
    else:
        data_dir = resolved(fallback_data_dir / "data")
    if not data_dir.is_dir():
        raise PipelineError(f"找不到兑换解压目录：{data_dir}")
    value["data_dir"] = str(data_dir)
    return value


def stage_error(manifest: Dict[str, Any], path: Path, stage: str, code: int, message: str) -> int:
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
    input_file = choose_input(args.input_file)
    runtime_value = args.runtime_dir
    if runtime_value is not None:
        runtime_dir = resolved(runtime_value)
    elif config.get("runtime_dir"):
        configured_runtime = config_path(config.get("runtime_dir"))
        runtime_dir = configured_runtime or resolved(DEFAULT_RUNTIME_DIR)
    else:
        runtime_dir = resolved(DEFAULT_RUNTIME_DIR)
    config_value = args.sub2api_config
    sub2api_config = resolved(config_value) if config_value else config_path(config.get("sub2api_config"))
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

    validate_layout(input_file, sub2api_config)
    print(f"卡密输入：{input_file}")
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
        "input_file": str(input_file),
        "run_dir": str(run_dir),
        "result_file": str(result_file),
        "redeem_manifest": str(redeem_manifest),
        "normalized_dir": str(normalized_dir),
        "skip_sub2api": bool(args.skip_sub2api),
        "skip_cockpit": bool(args.skip_cockpit),
    }
    try:
        (runtime_dir / "logs").mkdir(parents=True, exist_ok=True)
        write_json(pipeline_manifest, manifest)
        redeem_code = redeem_module.execute(
            input_file,
            redeem_dir,
            result_file,
            redeem_manifest,
        )
        manifest["redeem_exit_code"] = redeem_code
        manifest["redeem_result_file"] = str(result_file)
        if result_file.is_file():
            print(f"本次结果已覆盖写入：{result_file}")
        if redeem_code not in (0, 2):
            return stage_error(manifest, pipeline_manifest, "redeem", redeem_code, "兑换未完成")

        redeem_info = read_redeem_manifest(redeem_manifest, redeem_dir)
        manifest["redeem"] = {
            key: redeem_info.get(key)
            for key in ("task_id", "archive", "data_dir", "total", "success", "failed")
            if key in redeem_info
        }
        if redeem_code == 2:
            print("兑换结果包含失败卡密；将继续处理已下载的成功账号。")

        data_dir = Path(redeem_info["data_dir"])
        normalized = normalize_module.normalize(data_dir, normalized_dir)
        manifest["normalized"] = normalized
        write_json(pipeline_manifest, manifest)
        print(f"标准化完成：{normalized['accounts']} 个账号")

        if not args.skip_sub2api:
            sub2api_input = Path(normalized["sub2api_input"])
            sub2api_code = sub2api_module.execute(sub2api_input, sub2api_config)
            manifest["sub2api_exit_code"] = sub2api_code
            write_json(pipeline_manifest, manifest)
            if sub2api_code != 0:
                return stage_error(manifest, pipeline_manifest, "sub2api", sub2api_code, "Sub2API 导入未完成")
        else:
            print("已跳过 Sub2API 导入。")

        if not args.skip_cockpit:
            cockpit_input = Path(normalized["cockpit_input"])
            cockpit_code = cockpit_module.execute(cockpit_input, wait_seconds=wait_seconds)
            manifest["cockpit_exit_code"] = cockpit_code
            write_json(pipeline_manifest, manifest)
            if cockpit_code != 0:
                return stage_error(manifest, pipeline_manifest, "cockpit", cockpit_code, "Cockpit 导入未完成")
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
        redeem_module.RedeemModuleError,
        sub2api_module.Sub2ApiModuleError,
        cockpit_module.CockpitModuleError,
        PipelineError,
    ) as exc:
        return stage_error(manifest, pipeline_manifest, "pipeline", 1, str(exc))


def main() -> int:
    parser = argparse.ArgumentParser(
        description="兑换卡密、解压并标准化账号，然后导入 Sub2API 和 Cockpit"
    )
    parser.add_argument("input_file", nargs="?", type=Path, help="卡密文本；也可拖到 run.bat")
    parser.add_argument("--runtime-dir", type=Path, help="覆盖默认的 ProgramData 运行目录")
    parser.add_argument("--sub2api-config", type=Path, help="Sub2API 导入配置 JSON")
    parser.add_argument("--wait-seconds", type=int, help="Cockpit 导入等待秒数")
    parser.add_argument("--skip-sub2api", action="store_true", help="跳过 Sub2API 导入")
    parser.add_argument("--skip-cockpit", action="store_true", help="跳过 Cockpit 导入")
    parser.add_argument("--dry-run", action="store_true", help="只检查本地文件，不访问网络")
    args = parser.parse_args()
    try:
        return run(args)
    except (PipelineError, OSError, ValueError, TypeError, KeyError) as exc:
        print(f"流水线失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
