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
import incremental as incremental_module  # noqa: E402
from normalize import main as normalize_module  # noqa: E402
from redeem import main as redeem_module  # noqa: E402
from sub2api import main as sub2api_module  # noqa: E402
import token_state as token_state_module  # noqa: E402


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
    *,
    incremental: bool = False,
) -> bool:
    if sys.version_info < (3, 8):
        raise PipelineError("需要 Python 3.8 或更高版本")
    has_codes = False
    if codes_file is not None:
        has_codes = bool(
            redeem_module.read_codes(codes_file, allow_empty=incremental)
        )
    if accounts_file is not None:
        normalize_module.validate_input_file(accounts_file, allow_empty=True)
    if sub2api_config is not None:
        sub2api_module.validate_config(sub2api_config)
    return has_codes


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


def normalized_records(path: Path) -> list[Dict[str, Any]]:
    try:
        value = json.loads(path.read_text(encoding="utf-8-sig"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PipelineError(f"无法读取标准化账号：{path}") from exc
    if not isinstance(value, list) or not all(isinstance(item, dict) for item in value):
        raise PipelineError(f"标准化账号格式无效：{path}")
    return value


def managed_sub2api_ids(path: Path) -> Dict[str, int]:
    if not path.is_file():
        return {}
    try:
        value = json.loads(path.read_text(encoding="utf-8-sig"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise PipelineError(f"无法读取 Sub2API 增量映射：{path}") from exc
    accounts = value.get("accounts") if isinstance(value, dict) else None
    if not isinstance(accounts, dict):
        raise PipelineError(f"Sub2API 增量映射格式无效：{path}")
    result: Dict[str, int] = {}
    for key, account_id in accounts.items():
        try:
            number = int(account_id)
        except (TypeError, ValueError):
            continue
        if isinstance(key, str) and number > 0:
            result[key] = number
    return result


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
    refresh_tokens = bool(getattr(args, "refresh_tokens", False))
    if args.incremental and refresh_tokens:
        raise PipelineError("--incremental 不能与 --refresh-tokens 同时使用")
    if (args.incremental or refresh_tokens) and (
        args.skip_sub2api or args.skip_cockpit
    ):
        raise PipelineError(
            "增量和 Token 刷新模式需要同时同步 Sub2API 和 Cockpit，不能使用跳过选项"
        )
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

    has_codes = validate_layout(
        codes_file,
        accounts_file,
        sub2api_config,
        incremental=args.incremental or refresh_tokens,
    )
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
    incremental_dir = run_dir / "incremental"
    incremental_state_file = runtime_dir / "state" / "incremental.json"
    token_snapshot_file = runtime_dir / "state" / "token-snapshot.json"
    sub2api_summary_file = incremental_dir / "sub2api-accounts.json"
    result_file = runtime_dir / "results" / "redeem-result.txt"
    conflict_file = runtime_dir / "results" / "input-conflicts.txt"
    sub2api_pending_file = runtime_dir / "results" / "sub2api-pending-deletions.txt"
    cockpit_pending_file = runtime_dir / "results" / "cockpit-pending-deletions.txt"
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
        "incremental": bool(args.incremental),
        "refresh_tokens": refresh_tokens,
        "conflict_file": str(conflict_file),
    }
    stage = "redeem" if has_codes else "normalize"
    try:
        write_json(pipeline_manifest, manifest)
        redeem_code = 0
        data_dir: Optional[Path] = None
        refresh_keys: set[str] = set()
        if has_codes:
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
                for key in (
                    "task_id",
                    "archive",
                    "data_dir",
                    "total",
                    "success",
                    "failed",
                    "refresh_accounts",
                )
                if key in redeem_info
            }
            raw_refresh_keys = redeem_info.get("refresh_accounts", [])
            if not isinstance(raw_refresh_keys, list):
                raw_refresh_keys = []
            refresh_keys = set()
            for value in raw_refresh_keys:
                key = incremental_module.refresh_key(value)
                if key:
                    refresh_keys.add(key)
            if refresh_keys:
                print(
                    "[兑换] 检测到 "
                    f"{len(refresh_keys)} 个授权已更新账号；增量模式将强制重新导入。"
                )
            if redeem_code == 2:
                print("兑换结果包含失败卡密；将继续处理已下载的成功账号。")
        elif codes_file is not None:
            redeem_module.save_result(result_file, [])
            print("[兑换] 卡密文件没有启用的卡密；跳过兑换。")
        else:
            print("未提供卡密，跳过兑换阶段。")

        stage = "normalize"
        print("[流水线] 开始标准化账号数据……")
        normalized = normalize_module.normalize(
            data_dir,
            normalized_dir,
            accounts_file,
            allow_empty=(args.incremental or refresh_tokens) and not has_codes,
            conflict_file=conflict_file,
        )
        manifest["normalized"] = normalized
        write_json(pipeline_manifest, manifest)
        source_counts = normalized.get("source_counts", {})
        redeem_accounts = int(source_counts.get("redeem", 0) or 0)
        account_file_accounts = int(source_counts.get("accounts-file", 0) or 0)
        overlap_accounts = int(source_counts.get("overlap", 0) or 0)
        print(
            "[流水线] 输入合并："
            f"兑换来源 {redeem_accounts} 个，"
            f"账户文本 {account_file_accounts} 个，"
            f"重复 {overlap_accounts} 个，"
            f"合并后 {normalized['accounts']} 个。"
        )

        if refresh_tokens:
            stage = "token-refresh"
            records = normalized_records(Path(normalized["cockpit_input"]))
            token_snapshot = token_state_module.read_snapshot(token_snapshot_file)
            token_delta = token_state_module.compare(records, token_snapshot)
            print(
                "[Token] 明文快照比较完成："
                f"变化 {len(token_delta.changed)}，未变化 {token_delta.unchanged}。"
            )
            manifest["token_refresh"] = {
                "changed": len(token_delta.changed),
                "unchanged": token_delta.unchanged,
            }
            write_json(pipeline_manifest, manifest)

            if token_delta.changed:
                refresh_summary = incremental_dir / "token-refresh-sub2api.json"
                refresh_result = sub2api_module.refresh_credentials(
                    sub2api_config,
                    token_delta.changed,
                    summary_file=refresh_summary,
                )
                changed_by_key = {
                    token_state_module.account_key(record): record
                    for record in token_delta.changed
                }
                updated_records = [
                    changed_by_key[key]
                    for key in refresh_result.updated
                    if key in changed_by_key
                ]
                if updated_records:
                    cockpit_input = incremental_dir / "token-refresh-cockpit.json"
                    incremental_module.write_json(cockpit_input, updated_records)
                    print(
                        "[Token] 开始刷新 Cockpit "
                        f"（{len(updated_records)} 个账号）……"
                    )
                    cockpit_code = cockpit_module.execute(
                        cockpit_input, wait_seconds=wait_seconds
                    )
                    if cockpit_code != 0:
                        return stage_error(
                            manifest,
                            pipeline_manifest,
                            "token-refresh-cockpit",
                            cockpit_code,
                            "Cockpit token 刷新未完成",
                        )
                    print(f"[Token] 已向 Cockpit 提交 {len(updated_records)} 个账号。")
                    token_state_module.update_snapshot(
                        token_snapshot_file,
                        updated_records,
                        refresh_result.updated,
                    )

                incomplete = (
                    len(refresh_result.missing)
                    + len(refresh_result.manual)
                    + len(refresh_result.failed)
                )
                manifest["token_refresh"].update(
                    {
                        "updated": len(refresh_result.updated),
                        "missing": len(refresh_result.missing),
                        "manual": len(refresh_result.manual),
                        "failed": len(refresh_result.failed),
                    }
                )
                write_json(pipeline_manifest, manifest)
                if incomplete:
                    return stage_error(
                        manifest,
                        pipeline_manifest,
                        "token-refresh",
                        1,
                        f"有 {incomplete} 个账号未完成 token 刷新；日志未包含 token 内容",
                    )
            else:
                print("[Token] 没有 token 变化，跳过 Sub2API 和 Cockpit。")

            redeem_warning = redeem_code == 2
            manifest.update(
                {
                    "status": (
                        "success_with_redeem_warnings"
                        if redeem_warning
                        else "success"
                    ),
                    "finished_at": datetime.now().isoformat(timespec="seconds"),
                    "exit_code": 0,
                }
            )
            write_json(pipeline_manifest, manifest)
            print(f"Token 刷新完成。运行记录：{pipeline_manifest}")
            return 0

        incremental_delta = None
        baseline_sub2api_ids: Dict[str, int] = {}
        source_keys = normalized["source_keys"]
        source_scope = incremental_module.input_scope(codes_file, accounts_file)
        if args.incremental:
            records = normalized_records(Path(normalized["cockpit_input"]))
            incremental_state = incremental_module.read_state(incremental_state_file)
            incremental_delta = incremental_module.compare(
                records,
                incremental_state,
                source_scope,
                refresh_keys=refresh_keys,
            )
            is_baseline = (
                incremental_state.get("scope") is None
                or incremental_delta.scope_changed
            )
            if is_baseline:
                stage = "incremental-baseline"
                baseline_sub2api_ids = sub2api_module.resolve_account_ids(
                    sub2api_config, records, require_managed=True
                )
                baseline_keys = set(baseline_sub2api_ids)
                incremental_delta.current = {
                    key: record
                    for key, record in incremental_delta.current.items()
                    if key in baseline_keys
                }
                incremental_delta.added = []
                incremental_delta.refreshed = [
                    record
                    for key, record in incremental_delta.current.items()
                    if key in refresh_keys
                ]
                incremental_delta.removed = []
                incremental_delta.unchanged = (
                    len(incremental_delta.current) - len(incremental_delta.refreshed)
                )
                print(
                    "[增量] 首次运行建立安全基线；普通已有账号不导入，"
                    "授权更新账号仍会导入；"
                    f"已匹配 {len(baseline_sub2api_ids)} 个工具维护的 Sub2API 账户，"
                    f"忽略 {len(records) - len(baseline_sub2api_ids)} 个未带归属标记的账户。"
                )
            incremental_files = incremental_module.write_delta(
                incremental_dir, incremental_delta
            )
            if incremental_delta.scope_changed:
                print("[增量] 输入文件范围发生变化，本次按首次运行处理。")
            print(
                "[增量] 比较完成："
                f"新增 {len(incremental_delta.added)}，"
                f"授权更新 {len(incremental_delta.refreshed)}，"
                f"未变化 {incremental_delta.unchanged}，"
                f"输入中减少 {len(incremental_delta.removed)}（仅记录待手动处理，不自动删除）。"
            )
            for record in incremental_delta.refreshed:
                identity = (
                    incremental_module.text(record.get("email"))
                    or incremental_module.text(record.get("account_id"))
                    or "-"
                )
                print(f"[增量][授权更新] 将重新导入账号：{identity}")
            manifest["incremental_delta"] = {
                "added": len(incremental_delta.added),
                "refreshed": len(incremental_delta.refreshed),
                "removed": len(incremental_delta.removed),
                "unchanged": incremental_delta.unchanged,
                "scope_changed": incremental_delta.scope_changed,
            }
            write_json(pipeline_manifest, manifest)
            normalized = {**normalized, **incremental_files}

        if args.incremental and incremental_delta is not None:
            sub2api_pending_entries = [
                (record, "当前输入已移除；工具不会自动删除，请手动处理")
                for record in incremental_delta.removed
            ]
            sub2api_pending_count = incremental_module.update_sub2api_pending_deletions(
                sub2api_pending_file,
                sub2api_pending_entries,
                incremental_delta.current.values(),
            )
            cockpit_pending_count = incremental_module.update_pending_deletions(
                cockpit_pending_file,
                incremental_delta.removed,
                incremental_delta.current.values(),
            )
            manifest["manual_cleanup"] = {
                "detected": len(incremental_delta.removed),
                "sub2api_pending": sub2api_pending_count,
                "cockpit_pending": cockpit_pending_count,
                "sub2api_file": str(sub2api_pending_file),
                "cockpit_file": str(cockpit_pending_file),
            }
            write_json(pipeline_manifest, manifest)
            if incremental_delta.removed:
                print(
                    "[增量] 检测到 "
                    f"{len(incremental_delta.removed)} 个输入中减少的账号；"
                    "未调用任何删除接口。"
                )
                for record in incremental_delta.removed:
                    identity = (
                        incremental_module.text(record.get("email"))
                        or incremental_module.text(record.get("account_id"))
                        or "-"
                    )
                    raw_remote_id = record.get("sub2api_id")
                    remote_id = str(raw_remote_id).strip() if raw_remote_id else "-"
                    print(
                        f"[增量][待手动处理] 账号：{identity}；Sub2API ID：{remote_id}"
                    )
                print(
                    "[增量] Sub2API 待手动处理清单："
                    f"{sub2api_pending_count} 个，{sub2api_pending_file}"
                )
                print(
                    "[增量] Cockpit 待手动处理清单："
                    f"{cockpit_pending_count} 个，{cockpit_pending_file}"
                )
            elif sub2api_pending_count or cockpit_pending_count:
                print(
                    "[增量] 当前没有新的减少账号；"
                    f"仍有 Sub2API {sub2api_pending_count} 个、"
                    f"Cockpit {cockpit_pending_count} 个待手动处理。"
                )

        token_snapshot_records: list[Dict[str, Any]] = []
        token_snapshot_ids: Dict[str, int] = {}
        if not args.skip_sub2api:
            stage = "sub2api"
            import_count = len(normalized_records(Path(normalized["cockpit_input"])))
            if args.incremental and incremental_delta is not None and import_count == 0:
                print("[增量] 没有新增或授权更新账号，跳过 Sub2API 导入。")
                sub2api_code = 0
            else:
                if args.incremental:
                    print(
                        "[增量] 开始导入 Sub2API "
                        f"（新增 {len(incremental_delta.added)}，"
                        f"授权更新 {len(incremental_delta.refreshed)}，共 {import_count} 个）……"
                    )
                else:
                    print(
                        "[流水线] 全量模式：将导入全部 "
                        f"{import_count} 个标准化账号（不按增量快照跳过）。"
                    )
                sub2api_code = sub2api_module.execute(
                    Path(normalized["sub2api_input"]),
                    sub2api_config,
                    summary_file=sub2api_summary_file,
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
            cockpit_input = Path(normalized["cockpit_input"])
            cockpit_records = normalized_records(cockpit_input)
            normalized_cockpit_count = len(cockpit_records)
            if not args.skip_sub2api and cockpit_records:
                owned_ids = managed_sub2api_ids(sub2api_summary_file)
                cockpit_records = [
                    record
                    for record in cockpit_records
                    if incremental_module.account_key(record) in owned_ids
                ]
                managed_cockpit_input = cockpit_input.with_name(
                    "cockpit-managed-accounts.json"
                )
                incremental_module.write_json(managed_cockpit_input, cockpit_records)
                cockpit_input = managed_cockpit_input
                print(
                    "[Cockpit] 账号归属过滤："
                    f"标准化 {normalized_cockpit_count} 个，"
                    f"本工具可导入 {len(cockpit_records)} 个，"
                    f"跳过 {normalized_cockpit_count - len(cockpit_records)} 个。"
                )
            if not cockpit_records:
                if args.incremental:
                    reason = "本轮没有新增或授权更新账号，或账号未通过 Sub2API 归属校验"
                else:
                    reason = "没有可导入账号"
                print(f"[Cockpit] {reason}，跳过导入。")
                cockpit_code = 0
            else:
                print(
                    "[流水线] 开始导入 Cockpit "
                    f"（{len(cockpit_records)} 个账号，可能需要较长时间）……"
                )
                cockpit_code = cockpit_module.execute(
                    cockpit_input, wait_seconds=wait_seconds
                )
                print(f"[Cockpit] 已提交 {len(cockpit_records)} 个账号。")
            manifest["cockpit_exit_code"] = cockpit_code
            write_json(pipeline_manifest, manifest)
            if cockpit_code != 0:
                return stage_error(
                    manifest, pipeline_manifest, "cockpit", cockpit_code, "Cockpit 导入未完成"
                )
            if not args.skip_sub2api and cockpit_records:
                token_snapshot_records = cockpit_records
                token_snapshot_ids = managed_sub2api_ids(sub2api_summary_file)
        else:
            print("已跳过 Cockpit 导入。")

        if args.incremental and incremental_delta is not None:
            stage = "incremental-state"
            sub2api_ids = {
                key: item.get("sub2api_id")
                for key, item in incremental_delta.previous.items()
                if item.get("sub2api_id") is not None
            }
            sub2api_ids.update(baseline_sub2api_ids)
            summary_ids = managed_sub2api_ids(sub2api_summary_file)
            sub2api_ids.update(summary_ids)
            managed_current_keys = (
                set(incremental_delta.previous) & set(incremental_delta.current)
            )
            managed_current_keys.update(baseline_sub2api_ids)
            managed_current_keys.update(summary_ids)
            state_records = list(incremental_delta.current.values())
            state_source_keys = dict(source_keys)
            state_value = incremental_module.build_state(
                state_records,
                source_scope,
                sub2api_ids,
                managed_keys=managed_current_keys,
                source_keys=state_source_keys,
            )
            incremental_module.write_state(incremental_state_file, state_value)
            print(f"[增量] 快照已更新：{incremental_state_file}")

        if token_snapshot_records:
            token_state_module.update_snapshot(
                token_snapshot_file,
                token_snapshot_records,
                token_snapshot_ids,
            )
            print(
                "[Token] 已更新 "
                f"{len(token_snapshot_records)} 个账号的明文比较基线。"
            )

        redeem_warning = redeem_code == 2
        final_code = 0
        manifest.update(
            {
                "status": (
                    "success_with_redeem_warnings" if redeem_warning else "success"
                ),
                "finished_at": datetime.now().isoformat(timespec="seconds"),
                "exit_code": final_code,
        }
        )
        write_json(pipeline_manifest, manifest)
        if redeem_warning:
            message = "兑换结果含异常，已继续导入；异常仅记录。"
            print(f"流水线完成：{message}运行记录：{pipeline_manifest}")
        else:
            print(f"流水线完成。运行记录：{pipeline_manifest}")
        return final_code
    except (
        OSError,
        ValueError,
        TypeError,
        KeyError,
        incremental_module.IncrementalError,
        normalize_module.NormalizeError,
        redeem_module.RedeemError,
        sub2api_module.Sub2ApiError,
        cockpit_module.CockpitError,
        token_state_module.TokenStateError,
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
    parser.add_argument(
        "--codes-file", type=Path, help="卡密 TXT（一行一个，允许空行和卡密后注释）"
    )
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
    parser.add_argument(
        "--incremental",
        action="store_true",
        help="只导入新增或授权已更新的账号，并记录输入中减少的账号",
    )
    parser.add_argument(
        "--refresh-tokens",
        action="store_true",
        help="只刷新明文快照中发生变化的 token，不修改账户设置",
    )
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
        incremental_module.IncrementalError,
        normalize_module.NormalizeError,
        redeem_module.RedeemError,
        sub2api_module.Sub2ApiError,
        token_state_module.TokenStateError,
    ) as exc:
        print(f"流水线失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
