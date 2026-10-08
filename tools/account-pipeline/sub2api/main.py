"""使用 Sub2API Admin API 导入或更新 Codex 账号。"""

from __future__ import annotations

import argparse
import copy
import hashlib
import ipaddress
import json
import math
import os
import re
import socket
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence

try:
    from .cockpit_source import SourceError, load_records as load_cockpit_records
except ImportError:  # 直接执行 main.py 时没有包上下文
    from cockpit_source import SourceError, load_records as load_cockpit_records


MODULE_DIR = Path(__file__).resolve().parent
DEFAULT_RUNTIME_ENV = Path(r"C:\ProgramData\Sub2API\runtime\.env")
DEFAULT_CONFIG = MODULE_DIR / "config.example.json"
CANONICAL_CONFIG = Path(
    os.environ.get("PROGRAMDATA") or r"C:\ProgramData"
) / "Sub2API" / "account-pipeline" / "sub2api.json"
COMPAT_CONFIG = Path(r"C:\ProgramData\Sub2API\codex-account-import.json")
MAX_INPUT_BYTES = 32 * 1024 * 1024
MAX_RESPONSE_BYTES = 32 * 1024 * 1024
EMAIL_PATTERN = re.compile(r"(?i)[^@\s]+@[^@\s]+\.[^@\s]+")
OWNERSHIP_EXTRA_KEY = "account_pipeline_managed"
OWNERSHIP_EXTRA_VALUE = "account-pipeline-v1"
TOKEN_CREDENTIAL_FIELDS = ("access_token", "refresh_token", "id_token")
ACCOUNT_CONFIG_FIELDS = {
    "name_prefix", "name_start", "name_width", "group_names", "proxy_name",
    "concurrency", "priority", "rate_multiplier", "load_factor", "notes",
    "expires_at", "auto_pause_on_expired", "claim_existing_accounts", "extra",
}


class Sub2ApiError(RuntimeError):
    """Sub2API 导入错误。"""


@dataclass
class Batch:
    index: int
    config: Optional[Dict[str, Any]]
    input_path: Optional[Path]
    input_label: str
    source_type: str
    group_names: List[str]
    records: List[Dict[str, Any]]
    account_configs: Dict[str, Dict[str, Any]] = field(default_factory=dict)
    settings_only_keys: set[str] = field(default_factory=set)


@dataclass
class CredentialRefreshResult:
    updated: Dict[str, int] = field(default_factory=dict)
    missing: List[str] = field(default_factory=list)
    manual: List[str] = field(default_factory=list)
    failed: List[str] = field(default_factory=list)


def json_file(path: Path, label: str) -> Any:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise Sub2ApiError(f"无法读取 {label}：{path}") from exc
    if not raw:
        raise Sub2ApiError(f"{label}为空：{path}")
    if len(raw) > MAX_INPUT_BYTES:
        raise Sub2ApiError(f"{label}超过 {MAX_INPUT_BYTES // (1024 * 1024)} MiB")
    try:
        def unique_object(pairs):
            value = {}
            for key, item in pairs:
                if key in value:
                    raise Sub2ApiError(f"{label}包含重复 JSON 键：{path}")
                value[key] = item
            return value

        return json.loads(raw.decode("utf-8-sig"), object_pairs_hook=unique_object)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise Sub2ApiError(f"{label}不是有效的 UTF-8 JSON：{path}") from exc


def read_dotenv(path: Path) -> Dict[str, str]:
    if not path.is_file():
        raise Sub2ApiError(f"找不到 Sub2API 环境文件：{path}")
    values: Dict[str, str] = {}
    try:
        lines = path.read_text(encoding="utf-8-sig").splitlines()
    except (OSError, UnicodeDecodeError) as exc:
        raise Sub2ApiError(f"无法读取 Sub2API 环境文件：{path}") from exc
    for line in lines:
        match = re.match(r"^([A-Za-z_][A-Za-z0-9_]*)=(.*)$", line)
        if not match:
            continue
        value = match.group(2).strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        values[match.group(1)] = value
    return values


def optional_string(value: Any) -> Optional[str]:
    return value.strip() if isinstance(value, str) and value.strip() else None


def integer_value(value: Any, name: str) -> int:
    if isinstance(value, bool):
        raise Sub2ApiError(f"{name} 必须是整数")
    if isinstance(value, float) and not value.is_integer():
        raise Sub2ApiError(f"{name} 必须是整数")
    try:
        return int(value)
    except (TypeError, ValueError) as exc:
        raise Sub2ApiError(f"{name} 必须是整数") from exc


def number_value(value: Any, name: str) -> float:
    if isinstance(value, bool):
        raise Sub2ApiError(f"{name} 必须是数字")
    try:
        result = float(value)
    except (TypeError, ValueError) as exc:
        raise Sub2ApiError(f"{name} 必须是数字") from exc
    if not math.isfinite(result):
        raise Sub2ApiError(f"{name} 必须是有限数字")
    return result


def property_value(obj: Any, name: str, default: Any = None) -> Any:
    return obj.get(name, default) if isinstance(obj, dict) else default


def resolve_path(value: str, base: Path, label: str) -> Path:
    if not value.strip():
        raise Sub2ApiError(f"{label}不能为空")
    path = Path(value).expanduser()
    if not path.is_absolute():
        path = base / path
    path = path.resolve()
    if not path.is_file():
        raise Sub2ApiError(f"找不到账号 JSON：{path}")
    return path


def as_string_list(value: Any, label: str, *, required: bool = False) -> List[str]:
    if value is None:
        if required:
            raise Sub2ApiError(f"{label}必须填写（可以是空数组）")
        return []
    if not isinstance(value, list):
        raise Sub2ApiError(f"{label}必须是字符串数组")
    result = []
    for item in value:
        if not isinstance(item, str) or not item.strip():
            raise Sub2ApiError(f"{label}包含无效名称")
        result.append(item.strip())
    return result


def read_records(
    path: Path, source_type: str, *,
    account_configs: Optional[Dict[str, Dict[str, Any]]] = None,
    settings_only_keys: Optional[set[str]] = None,
) -> List[Dict[str, Any]]:
    source = json_file(path, "账号 JSON")
    if isinstance(source, dict) and "accounts" in source:
        raw = source["accounts"]
        if not isinstance(raw, list):
            raise Sub2ApiError(f"账号 JSON 的 accounts 必须是数组：{path}")
        records = []
        for item in raw:
            if not isinstance(item, dict) or not isinstance(item.get("credentials"), dict):
                continue
            records.append(item["credentials"])
            settings_only = item.get("settings_only", False)
            if not isinstance(settings_only, bool):
                raise Sub2ApiError(f"settings_only 必须是 true 或 false：{path}")
            if settings_only:
                key = record_key(item["credentials"])
                if key is None:
                    raise Sub2ApiError(f"仅更新设置的账号缺少 email/account_id：{path}")
                if settings_only_keys is not None:
                    settings_only_keys.add(key)
            if "config" in item:
                settings = item["config"]
                if not isinstance(settings, dict):
                    raise Sub2ApiError(f"账号配置必须是 JSON 对象：{path}")
                key = record_key(item["credentials"])
                if key is None:
                    raise Sub2ApiError(f"账号配置缺少 email/account_id：{path}")
                if account_configs is not None:
                    account_configs[key] = settings
    elif isinstance(source, list):
        records = source
    elif isinstance(source, dict):
        records = [source]
    else:
        records = []

    filtered: List[Dict[str, Any]] = []
    for record in records:
        if not isinstance(record, dict):
            continue
        record_type = str(record.get("type", ""))
        if source_type and record_type.lower() != source_type.lower():
            continue
        if not optional_string(record.get("access_token")) and not optional_string(
            record.get("accessToken")
        ):
            raise Sub2ApiError(f"账号 JSON 中存在缺少 access_token/accessToken 的记录：{path}")
        filtered.append(copy.deepcopy(record))
    if not filtered:
        raise Sub2ApiError(
            f"账号 JSON 中没有符合 source_type='{source_type}' 的记录：{path}"
        )
    return filtered


def effective(batch: Optional[Dict[str, Any]], config: Dict[str, Any], name: str, default: Any = None) -> Any:
    if isinstance(batch, dict) and name in batch:
        return batch[name]
    return config.get(name, default)


def build_batches(
    config: Dict[str, Any],
    config_dir: Path,
    input_file: Optional[Path],
    cockpit_tools: bool,
) -> List[Batch]:
    if input_file is not None and cockpit_tools:
        raise Sub2ApiError("--cockpit-tools 不能与输入文件同时使用")
    source_type = optional_string(config.get("source_type")) or "codex"
    configured_batches = config.get("batches")
    if input_file is None and not cockpit_tools and configured_batches is not None:
        if not isinstance(configured_batches, list) or not configured_batches:
            raise Sub2ApiError("batches 至少需要包含一个批次")
        batches = []
        for number, raw_batch in enumerate(configured_batches, start=1):
            if not isinstance(raw_batch, dict):
                raise Sub2ApiError(f"第 {number} 个批次不能为空")
            batch_path = raw_batch.get("input_path")
            if not isinstance(batch_path, str) or not batch_path.strip():
                raise Sub2ApiError(f"第 {number} 个批次必须填写 input_path")
            groups = as_string_list(raw_batch.get("group_names"), f"第 {number} 个批次的 group_names", required=True)
            path = resolve_path(batch_path, config_dir, f"第 {number} 个批次的 input_path")
            batch_source = optional_string(
                effective(raw_batch, config, "source_type", source_type)
            ) or ""
            account_configs: Dict[str, Dict[str, Any]] = {}
            settings_only_keys: set[str] = set()
            records = read_records(path, batch_source, account_configs=account_configs,
                                   settings_only_keys=settings_only_keys)
            batches.append(
                Batch(number, raw_batch, path, str(path), batch_source, groups, records,
                      account_configs=account_configs, settings_only_keys=settings_only_keys)
            )
        return batches

    account_configs = {}
    settings_only_keys = set()
    if cockpit_tools:
        try:
            records = load_cockpit_records()
        except SourceError as exc:
            raise Sub2ApiError(str(exc)) from exc
        label = "Cockpit Tools 本地数据"
        path = None
    else:
        configured_input = config.get("input_path", "")
        if input_file is not None:
            path = input_file.expanduser().resolve()
        else:
            if not isinstance(configured_input, str) or not configured_input.strip():
                raise Sub2ApiError("未提供账号 JSON；请拖入 BAT、填写 input_path 或使用 --cockpit-tools")
            path = resolve_path(configured_input, config_dir, "input_path")
        if not path.is_file():
            raise Sub2ApiError(f"找不到账号 JSON：{path}")
        records = read_records(path, source_type, account_configs=account_configs,
                               settings_only_keys=settings_only_keys)
        label = str(path)
    groups = as_string_list(config.get("defaults", {}).get("group_names", config.get("group_names", [])), "group_names")
    return [Batch(1, None, path, label, source_type, groups, records,
                  account_configs=account_configs, settings_only_keys=settings_only_keys)]


def loopback_url(value: str, runtime: Dict[str, str]) -> str:
    if not value:
        port = runtime.get("SERVER_PORT", "18080")
        value = f"http://127.0.0.1:{port}"
    parsed = urllib.parse.urlsplit(value)
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        raise Sub2ApiError("sub2api_url 必须是有效的 http/https 地址")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise Sub2ApiError("sub2api_url 不能包含用户信息、查询参数或片段")
    host = parsed.hostname.lower()
    is_loopback = host == "localhost"
    if not is_loopback:
        try:
            is_loopback = ipaddress.ip_address(host).is_loopback
        except ValueError:
            is_loopback = False
    if not is_loopback:
        raise Sub2ApiError("sub2api_url 只允许本机回环地址，避免把管理员密码发送到其他主机")
    try:
        parsed.port
    except ValueError as exc:
        raise Sub2ApiError("sub2api_url 端口无效") from exc
    return value.rstrip("/")


class NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    """管理员 API 不允许把凭据或令牌跟随重定向发往其他地址。"""

    def redirect_request(self, request, fp, code, msg, headers, new_url):
        raise Sub2ApiError("Sub2API API 不允许重定向")


class ApiClient:
    def __init__(self, base_url: str):
        self.base_url = base_url
        self.opener = urllib.request.build_opener(NoRedirectHandler)

    def request(
        self,
        method: str,
        path: str,
        *,
        token: Optional[str] = None,
        body: Any = None,
    ) -> Any:
        data = None
        headers = {"Accept": "application/json"}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        if body is not None:
            data = json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(
            self.base_url + "/api/v1" + path,
            data=data,
            headers=headers,
            method=method,
        )
        try:
            with self.opener.open(request, timeout=120) as response:
                raw = response.read(MAX_RESPONSE_BYTES + 1)
        except urllib.error.HTTPError as exc:
            exc.close()
            raise Sub2ApiError(f"请求 Sub2API {method} {path} 失败（HTTP {exc.code}）") from exc
        except (urllib.error.URLError, TimeoutError, socket.timeout, OSError) as exc:
            raise Sub2ApiError(f"请求 Sub2API {method} {path} 失败；服务器响应未输出") from exc
        if len(raw) > MAX_RESPONSE_BYTES:
            raise Sub2ApiError("Sub2API 响应超过大小限制")
        try:
            response_value = json.loads(raw.decode("utf-8-sig"))
        except (UnicodeDecodeError, json.JSONDecodeError) as exc:
            raise Sub2ApiError("Sub2API 返回了无效 JSON；服务器响应未输出") from exc
        if isinstance(response_value, dict) and response_value.get("code") not in (None, 0, "0"):
            raise Sub2ApiError("Sub2API 返回业务错误；服务器响应未输出")
        if isinstance(response_value, dict) and "data" in response_value:
            return response_value["data"]
        return response_value


def resolve_unique(items: Sequence[Dict[str, Any]], name: str, label: str) -> Dict[str, Any]:
    matches = [
        item for item in items
        if isinstance(item, dict) and str(item.get("name", "")).lower() == name.lower()
    ]
    if len(matches) != 1:
        raise Sub2ApiError(f"{label} '{name}' 匹配到 {len(matches)} 条记录，请使用唯一名称")
    return matches[0]


def get_accounts(client: ApiClient, token: str) -> List[Dict[str, Any]]:
    accounts: List[Dict[str, Any]] = []
    page = 1
    while True:
        data = client.request(
            "GET",
            f"/admin/accounts?page={page}&page_size=100&platform=openai&type=oauth",
            token=token,
        )
        if not isinstance(data, dict):
            raise Sub2ApiError("Sub2API 账户列表格式无效")
        items = data.get("items", [])
        if not isinstance(items, list):
            raise Sub2ApiError("Sub2API 账户列表 items 格式无效")
        accounts.extend(item for item in items if isinstance(item, dict))
        pages = data.get("pages", 1)
        try:
            pages = max(1, int(pages))
        except (TypeError, ValueError) as exc:
            raise Sub2ApiError("Sub2API 账户列表 pages 格式无效") from exc
        page += 1
        if page > pages:
            return accounts


def record_key(record: Mapping[str, Any]) -> Optional[str]:
    email = optional_string(record.get("email"))
    if email:
        return "email:" + email.casefold()
    account_id = optional_string(record.get("account_id"))
    return "id:" + account_id.casefold() if account_id else None


def record_keys(record: Mapping[str, Any]) -> List[str]:
    values: List[str] = []
    primary = record_key(record)
    if primary:
        values.append(primary)
    account_id = optional_string(record.get("account_id"))
    if account_id:
        key = "id:" + account_id.casefold()
        if key not in values:
            values.append(key)
    email = optional_string(record.get("email"))
    if email:
        key = "email:" + email.casefold()
        if key not in values:
            values.append(key)
    return values


def account_emails(account: Mapping[str, Any]) -> List[str]:
    values = []
    for key in ("email", "name"):
        value = optional_string(account.get(key))
        if value and EMAIL_PATTERN.fullmatch(value):
            values.append(value)
    credentials = account.get("credentials")
    if isinstance(credentials, str):
        try:
            credentials = json.loads(credentials)
        except (TypeError, ValueError):
            credentials = None
    if isinstance(credentials, dict):
        value = optional_string(credentials.get("email"))
        if value and EMAIL_PATTERN.fullmatch(value):
            values.append(value)
    return list(dict.fromkeys(values))


def account_extra(account: Mapping[str, Any]) -> Dict[str, Any]:
    value = account.get("extra")
    if isinstance(value, dict):
        return value
    if isinstance(value, str) and value.strip():
        try:
            decoded = json.loads(value)
        except (TypeError, ValueError):
            return {}
        return decoded if isinstance(decoded, dict) else {}
    return {}


def is_tool_managed(account: Mapping[str, Any]) -> bool:
    return account_extra(account).get(OWNERSHIP_EXTRA_KEY) == OWNERSHIP_EXTRA_VALUE


def account_keys(account: Mapping[str, Any]) -> List[str]:
    values = ["email:" + email.casefold() for email in account_emails(account)]
    sources: List[Mapping[str, Any]] = [account]
    credentials = account.get("credentials")
    if isinstance(credentials, str):
        try:
            credentials = json.loads(credentials)
        except (TypeError, ValueError):
            credentials = None
    if isinstance(credentials, dict):
        sources.append(credentials)
    for source in sources:
        for field_name in ("email", "user_email", "account_email", "accountEmail"):
            email = optional_string(source.get(field_name))
            if email:
                values.append("email:" + email.casefold())
        for field_name in ("account_id", "accountId", "chatgpt_account_id"):
            value = optional_string(source.get(field_name))
            if value:
                values.append("id:" + value.casefold())
    return list(dict.fromkeys(values))


def account_database_id(account: Mapping[str, Any]) -> int:
    try:
        value = int(account.get("id", 0) or 0)
    except (TypeError, ValueError):
        return 0
    return value if value > 0 else 0


def account_detail(
    client: ApiClient,
    token: str,
    account_id: int,
) -> Dict[str, Any]:
    value = client.request("GET", f"/admin/accounts/{account_id}", token=token)
    if not isinstance(value, dict) or account_database_id(value) != account_id:
        raise Sub2ApiError(f"Sub2API 账户 ID {account_id} 的明细格式无效")
    return value


def refresh_credentials(
    config_file: Optional[Path],
    records: Iterable[Dict[str, Any]],
    *,
    summary_file: Optional[Path] = None,
) -> CredentialRefreshResult:
    """只更新工具维护账户的凭据，不提交任何账户设置字段。"""
    record_list = list(records)
    result = CredentialRefreshResult()
    if not record_list:
        if summary_file is not None:
            write_managed_summary(summary_file, {})
        return result

    _, _, client, token = admin_session(config_file)
    remote_index = account_index(get_accounts(client, token))
    for record in record_list:
        key = record_key(record)
        if key is None:
            raise Sub2ApiError("token 刷新账号缺少 email/account_id")
        identity = optional_string(record.get("email")) or optional_string(
            record.get("account_id")
        ) or key
        account = matching_account(remote_index, record)
        if account is None:
            result.missing.append(identity)
            print(f"[Token][跳过] Sub2API 中找不到账号：{identity}")
            continue
        account_id = account_database_id(account)
        detail = account_detail(client, token, account_id)
        if not set(record_keys(record)).intersection(account_keys(detail)):
            raise Sub2ApiError(f"账号 {identity} 的远端标识已变化；已停止 token 刷新")
        if not is_tool_managed(detail):
            result.manual.append(identity)
            print(f"[Token][跳过] 账号不属于自动化维护：{identity}（ID {account_id}）")
            continue

        credentials: Dict[str, str] = {}
        for field_name in TOKEN_CREDENTIAL_FIELDS:
            value = optional_string(record.get(field_name))
            if value:
                credentials[field_name] = value
        try:
            updated = client.request(
                "POST",
                "/admin/accounts/bulk-update",
                token=token,
                body={"account_ids": [account_id], "credentials": credentials},
            )
            if not isinstance(updated, dict):
                raise Sub2ApiError("更新接口返回格式无效")
            if result_count(updated, "success") != 1 or result_count(
                updated, "failed"
            ):
                raise Sub2ApiError("更新接口未确认账号凭据已更新")
            verified = account_detail(client, token, account_id)
            if not is_tool_managed(verified) or key not in account_keys(verified):
                raise Sub2ApiError("更新后的账户归属或标识校验失败")
        except Sub2ApiError:
            result.failed.append(identity)
            print(f"[Token][失败] Sub2API 凭据未更新：{identity}")
            continue
        result.updated[key] = account_id
        print(f"[Token][完成] 已更新 Sub2API 凭据：{identity}（ID {account_id}）")

    if summary_file is not None:
        write_managed_summary(summary_file, result.updated)
    print(
        "[Token] Sub2API 更新完成："
        f"成功 {len(result.updated)}，找不到 {len(result.missing)}，"
        f"手动账户 {len(result.manual)}，失败 {len(result.failed)}。"
    )
    return result


def write_managed_summary(path: Path, account_ids: Dict[str, int]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    try:
        temporary.write_text(
            json.dumps({"version": 2, "accounts": account_ids}, ensure_ascii=False, indent=2)
            + "\n",
            encoding="utf-8",
        )
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise Sub2ApiError(f"写入 Sub2API 增量映射失败：{path}") from exc


def account_index(accounts: Iterable[Dict[str, Any]]) -> Dict[str, List[Dict[str, Any]]]:
    index: Dict[str, List[Dict[str, Any]]] = {}
    for account in accounts:
        for key in account_keys(account):
            index.setdefault(key, []).append(account)
    return index


def matching_account(
    index: Mapping[str, Sequence[Dict[str, Any]]], record: Mapping[str, Any]
) -> Optional[Dict[str, Any]]:
    matches: Dict[int, Dict[str, Any]] = {}
    for key in record_keys(record):
        for account in index.get(key, ()):
            account_id = account_database_id(account)
            if account_id > 0:
                matches[account_id] = account
    if len(matches) > 1:
        ids = ", ".join(str(value) for value in sorted(matches))
        raise Sub2ApiError(f"账号标识匹配到多个 Sub2API 账户（ID：{ids}），已停止以避免误更新")
    return next(iter(matches.values()), None)


def comparable(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def result_count(result: Mapping[str, Any], name: str) -> int:
    try:
        return int(result.get(name, 0) or 0)
    except (TypeError, ValueError) as exc:
        raise Sub2ApiError("Sub2API API 结果格式无效") from exc


def safe_api_message(value: Any) -> str:
    """保留服务端诊断文本，同时隐藏可能误返回的凭据。"""
    if not isinstance(value, str):
        return ""
    text = re.sub(r"[\x00-\x1f\x7f]", " ", value).strip()
    text = re.sub(
        r"(?i)\b(?:eyJ[\w-]*\.[\w-]+\.[\w-]+|rt\.[A-Za-z0-9._-]{20,})\b",
        "<已隐藏>",
        text,
    )
    text = re.sub(
        r'''(?i)(\b(?:(?:access|refresh|id)[_-]?token|password|passwd|secret|api[_-]?key|authorization)'''
        r'''\b["']?\s*(?:=|:)\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|(?:Bearer|Basic)\s+[^\s,;}]+|[^\s,;}]+)''',
        r"\1<已隐藏>",
        text,
    )
    text = re.sub(r"(?i)(https?://)[^/\s@]+@", r"\1<已隐藏>@", text)
    text = re.sub(r"\b[A-Za-z0-9_-]{32,}\b", "<已隐藏>", text)
    return text[:240]


def failed_item_message(result: Mapping[str, Any], index: int) -> str:
    """从批量导入结果提取指定账号的脱敏失败原因。"""
    candidates = []
    for key in ("items", "errors"):
        values = result.get(key, [])
        if not isinstance(values, list):
            continue
        candidates.extend(
            item
            for item in values
            if isinstance(item, dict) and item.get("index") in (None, index)
        )
    for item in candidates:
        for key in ("message", "error", "detail"):
            message = safe_api_message(item.get(key))
            if message:
                return message
    return ""


def snapshot(account: Dict[str, Any], managed_extra: Iterable[str]) -> Dict[str, Any]:
    extra = account_extra(account)
    credentials = {
        "values": property_value(account, "credentials"),
        "status": property_value(account, "credentials_status"),
        "token_hash": extra.get("access_token_sha256", ""),
    }
    raw_group_ids = property_value(account, "group_ids", [])
    if not isinstance(raw_group_ids, (list, tuple, set)):
        raw_group_ids = []
    group_ids = []
    for value in raw_group_ids:
        try:
            group_ids.append(int(value))
        except (TypeError, ValueError):
            continue
    fields: Dict[str, str] = {
        "凭据": comparable(credentials),
        "代理": comparable(property_value(account, "proxy_id")),
        "并发数": comparable(property_value(account, "concurrency")),
        "优先级": comparable(property_value(account, "priority")),
        "账户倍率": comparable(property_value(account, "rate_multiplier")),
        "负载因子": comparable(property_value(account, "load_factor")),
        "分组": comparable(sorted(group_ids)),
        "到期时间": comparable(property_value(account, "expires_at")),
        "到期自动暂停": comparable(property_value(account, "auto_pause_on_expired")),
    }
    for name in managed_extra:
        fields[f"extra.{name}"] = comparable(extra.get(name))
    try:
        account_id = int(property_value(account, "id", 0))
    except (TypeError, ValueError):
        account_id = 0
    return {"id": account_id, "name": str(property_value(account, "name", "")), "fields": fields}


def changed_fields(before: Dict[str, Any], after: Dict[str, Any]) -> List[str]:
    return [
        name for name, value in after["fields"].items()
        if before["fields"].get(name) != value
    ]


def account_label(
    account_snapshot: Optional[Dict[str, Any]],
    account_id: int,
    fallback: str,
    batch_index: int,
    record_index: int,
) -> str:
    name = (
        str(account_snapshot.get("name", ""))
        if account_snapshot is not None
        else fallback
    )
    name = re.sub(r"[\x00-\x1f\x7f]", " ", name).strip()
    if EMAIL_PATTERN.search(name):
        name = ""
    if name and account_id > 0:
        return f"{name}（ID {account_id}）"
    if account_id > 0:
        return f"ID {account_id}"
    if name:
        return name
    return f"批次 {batch_index} 第 {record_index} 个账户"


def expires_timestamp(value: Any) -> Optional[int]:
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip().replace("Z", "+00:00")
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError as exc:
        raise Sub2ApiError("expires_at 必须是有效的 ISO 8601 时间") from exc
    if parsed.tzinfo is None:
        raise Sub2ApiError("expires_at 必须包含时区")
    return int(parsed.timestamp())


def account_settings(config: Mapping[str, Any]) -> Dict[str, Any]:
    """在写入前校验账户设置；不读取登录凭据。"""
    settings = {
        "name_prefix": str(config.get("name_prefix", "") or ""),
        "name_start": integer_value(config.get("name_start", 1), "name_start"),
        "name_width": integer_value(config.get("name_width", 3), "name_width"),
        "group_names": sorted(set(as_string_list(config.get("group_names", []), "group_names"))),
        "proxy_name": str(config.get("proxy_name", "") or "").strip(),
        "concurrency": integer_value(config.get("concurrency", 3), "concurrency"),
        "priority": integer_value(config.get("priority", 50), "priority"),
        "rate_multiplier": number_value(config.get("rate_multiplier", 1), "rate_multiplier"),
        "load_factor": config.get("load_factor"),
        "notes": str(config.get("notes", "") or ""),
        "expires_at": config.get("expires_at", ""),
        "auto_pause_on_expired": config.get("auto_pause_on_expired", False),
        "claim_existing_accounts": config.get("claim_existing_accounts", False),
    }
    if settings["name_start"] < 0:
        raise Sub2ApiError("name_start 不能小于 0")
    if not 1 <= settings["name_width"] <= 99:
        raise Sub2ApiError("name_width 必须在 1 到 99 之间")
    for name in ("concurrency", "priority"):
        if settings[name] < 1:
            raise Sub2ApiError(f"{name} 必须大于 0")
    if settings["rate_multiplier"] < 0:
        raise Sub2ApiError("rate_multiplier 不能小于 0")
    if settings["load_factor"] is not None:
        settings["load_factor"] = integer_value(settings["load_factor"], "load_factor")
        if settings["load_factor"] < 1:
            raise Sub2ApiError("load_factor 必须大于 0，或设为 null 使用默认值")
    for name in ("auto_pause_on_expired", "claim_existing_accounts"):
        if not isinstance(settings[name], bool):
            raise Sub2ApiError(f"{name} 必须是 true 或 false")
    expires_timestamp(settings["expires_at"])
    configured_extra = config.get("extra") or {}
    if not isinstance(configured_extra, dict):
        raise Sub2ApiError("extra 必须是 JSON 对象")
    extra = dict(configured_extra)
    if extra.get(OWNERSHIP_EXTRA_KEY) not in (None, OWNERSHIP_EXTRA_VALUE):
        raise Sub2ApiError(
            f"extra.{OWNERSHIP_EXTRA_KEY} 是保留字段，必须使用 {OWNERSHIP_EXTRA_VALUE}"
        )
    extra[OWNERSHIP_EXTRA_KEY] = OWNERSHIP_EXTRA_VALUE
    settings["extra"] = extra
    return settings


class AccountConfig:
    """默认设置、兑换码覆盖和邮箱覆盖的唯一解析入口。"""

    def __init__(self, config: Mapping[str, Any]):
        defaults = config.get("defaults", {})
        self._validate_override(defaults, "defaults")
        self.defaults = account_settings({
            **{name: value for name, value in config.items() if name in ACCOUNT_CONFIG_FIELDS},
            **defaults,
        })
        self.accounts: Dict[str, Dict[str, Any]] = {}
        self.redeem_codes: Dict[str, Dict[str, Any]] = {}
        for field_name, target in (("accounts", self.accounts), ("redeem_codes", self.redeem_codes)):
            entries = config.get(field_name, {})
            if not isinstance(entries, dict):
                raise Sub2ApiError(f"{field_name} 必须是 JSON 对象")
            for identifier, override in entries.items():
                if not isinstance(identifier, str) or not identifier.strip():
                    raise Sub2ApiError(f"{field_name} 包含空标识")
                identifier = identifier.strip()
                if field_name == "accounts":
                    if not EMAIL_PATTERN.fullmatch(identifier):
                        raise Sub2ApiError("accounts 的键必须是账户邮箱")
                    key = identifier.casefold()
                else:
                    if any(char.isspace() for char in identifier):
                        raise Sub2ApiError("redeem_codes 的键必须是单个兑换码")
                    key = hashlib.sha256(identifier.encode("utf-8")).hexdigest()
                if key in target:
                    raise Sub2ApiError(f"{field_name} 包含重复标识")
                self._validate_override(override, field_name)
                settings = account_settings({
                    **self.defaults, **override,
                    "extra": {**self.defaults["extra"], **override.get("extra", {})},
                })
                target[key] = {
                    name: copy.deepcopy(value if name == "extra" else settings[name])
                    for name, value in override.items()
                }

    @staticmethod
    def _validate_override(value: Any, label: str) -> None:
        if not isinstance(value, dict):
            raise Sub2ApiError(f"{label} 必须是 JSON 对象")
        if value.keys() - ACCOUNT_CONFIG_FIELDS:
            raise Sub2ApiError(f"{label} 包含不支持的账户设置字段")
        if "extra" in value and not isinstance(value["extra"], dict):
            raise Sub2ApiError(f"{label}.extra 必须是 JSON 对象")

    def resolve(self, email: str, overrides: Optional[Mapping[str, Any]] = None) -> Dict[str, Any]:
        overrides = overrides or {}
        account = self.accounts.get(email.strip().casefold(), {})
        return account_settings({
            **self.defaults, **overrides, **account,
            "extra": {**self.defaults["extra"], **(overrides.get("extra") or {}), **account.get("extra", {})},
        })

    def compile(
        self, records: Sequence[Dict[str, Any]], *, active_codes: Iterable[str] = (),
        code_accounts: Optional[Mapping[str, Optional[str]]] = None,
    ) -> tuple[Dict[str, Dict[str, Any]], Dict[str, str]]:
        if code_accounts is not None and not isinstance(code_accounts, dict):
            raise Sub2ApiError("兑换码与账户的对应关系必须是 JSON 对象")
        emails = {str(record.get("email", "")).strip().casefold() for record in records}
        code_overrides: Dict[str, Dict[str, Any]] = {}
        for code in active_codes:
            digest = hashlib.sha256(code.encode("utf-8")).hexdigest()
            if digest not in self.redeem_codes:
                continue
            if code_accounts is None or digest not in code_accounts:
                raise Sub2ApiError("兑换结果缺少已配置卡密的账户对应关系；已停止导入")
            email = code_accounts[digest]
            if email is None:
                continue  # 失败卡密没有账户，不影响其他成功输入。
            if not isinstance(email, str) or email.strip().casefold() not in emails:
                raise Sub2ApiError("已配置卡密的账户无法匹配下载内容；已停止导入")
            email = email.strip().casefold()
            combined = code_overrides.setdefault(email, {})
            account = self.accounts.get(email, {})
            for name, value in self.redeem_codes[digest].items():
                if name == "group_names":
                    combined[name] = sorted(set(combined.get(name, [])) | set(value))
                    continue
                values = value.items() if name == "extra" else [(name, value)]
                target = combined.setdefault("extra", {}) if name == "extra" else combined
                final = account.get("extra", {}) if name == "extra" else account
                for field_name, field_value in values:
                    if field_name in target and comparable(target[field_name]) != comparable(field_value) and field_name not in final:
                        field_label = f"extra.{field_name}" if name == "extra" else field_name
                        raise Sub2ApiError(f"账户 {email} 的兑换码配置冲突：{field_label}；已停止导入")
                    target[field_name] = field_value
        configs, fingerprints = {}, {}
        for index, record in enumerate(records):
            key = record_key(record)
            if key is None:
                raise Sub2ApiError("账户缺少 email/account_id，无法解析配置")
            email = str(record.get("email", "")).strip()
            settings = self.resolve(email, code_overrides.get(email.casefold()))
            prefix = settings["name_prefix"]
            settings["name"] = (
                prefix + f"{settings['name_start'] + index:0{settings['name_width']}d}" if prefix else email
            )
            configs[key] = settings
            fingerprints[key] = hashlib.sha256(comparable(settings).encode("utf-8")).hexdigest()
        return configs, fingerprints


def admin_session(
    config_file: Optional[Path],
) -> tuple[Path, Dict[str, Any], ApiClient, str]:
    config_path = resolve_config(config_file)
    config_value = json_file(config_path, "Sub2API 导入配置")
    if not isinstance(config_value, dict):
        raise Sub2ApiError("Sub2API 导入配置必须是 JSON 对象")
    runtime_env_text = optional_string(
        config_value.get("runtime_env_path", str(DEFAULT_RUNTIME_ENV))
    )
    runtime_env = Path(runtime_env_text or str(DEFAULT_RUNTIME_ENV)).expanduser()
    if not runtime_env.is_absolute():
        runtime_env = config_path.parent / runtime_env
    runtime = read_dotenv(runtime_env.resolve())
    if not runtime.get("ADMIN_EMAIL") or not runtime.get("ADMIN_PASSWORD"):
        raise Sub2ApiError("Sub2API 环境文件缺少 ADMIN_EMAIL 或 ADMIN_PASSWORD")
    client = ApiClient(loopback_url(str(config_value.get("sub2api_url", "")), runtime))
    login = client.request(
        "POST",
        "/auth/login",
        body={"email": runtime["ADMIN_EMAIL"], "password": runtime["ADMIN_PASSWORD"]},
    )
    token = optional_string(property_value(login, "access_token"))
    if not token:
        raise Sub2ApiError(
            "管理员登录未返回 access_token；如已启用二次验证，请先在 Sub2API 中完成登录"
        )
    return config_path, config_value, client, token


def run_import(
    input_file: Optional[Path],
    config_file: Optional[Path],
    *,
    cockpit_tools: bool = False,
    what_if: bool = False,
    summary_file: Optional[Path] = None,
) -> int:
    config_path, config_value, client, token = admin_session(config_file)
    account_config = AccountConfig(config_value)
    config_dir = config_path.parent
    batches = build_batches(config_value, config_dir, input_file, cockpit_tools)

    groups_value = client.request("GET", "/admin/groups/all?include_inactive=true", token=token)
    groups = groups_value if isinstance(groups_value, list) else []
    if not isinstance(groups_value, list):
        raise Sub2ApiError("Sub2API 分组列表格式无效")
    prepared: Dict[tuple[int, int], Dict[str, Any]] = {}
    proxies: Optional[List[Dict[str, Any]]] = None
    for batch in batches:
        for index, record in enumerate(batch.records):
            key = record_key(record)
            if key is None:
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号缺少 email/account_id")
            overrides = batch.account_configs.get(key, {})
            base = account_config.resolve(str(record.get("email", "")), {
                "group_names": batch.group_names, **(batch.config or {}),
            })
            combined = {**base, **overrides}
            combined["extra"] = {
                **base["extra"],
                **(overrides.get("extra") or {}),
            }
            settings = account_settings(combined)
            settings["group_ids"] = []
            for group_name in settings["group_names"]:
                group = resolve_unique(groups, group_name, "分组")
                if group.get("platform") != "openai" or group.get("status") != "active":
                    raise Sub2ApiError(f"分组 '{group_name}' 必须是启用状态的 OpenAI 分组")
                try:
                    settings["group_ids"].append(int(group["id"]))
                except (KeyError, TypeError, ValueError) as exc:
                    raise Sub2ApiError(f"分组 '{group_name}' 的 ID 无效") from exc
            settings["proxy_id"] = None
            proxy_name = settings["proxy_name"]
            if proxy_name:
                if proxies is None:
                    proxies = client.request("GET", "/admin/proxies/all", token=token)
                    if not isinstance(proxies, list):
                        raise Sub2ApiError("Sub2API 代理列表格式无效")
                proxy = resolve_unique(proxies, proxy_name, "代理")
                if proxy.get("status") != "active":
                    raise Sub2ApiError(f"代理 '{proxy_name}' 当前未启用")
                try:
                    settings["proxy_id"] = int(proxy["id"])
                except (KeyError, TypeError, ValueError) as exc:
                    raise Sub2ApiError(f"代理 '{proxy_name}' 的 ID 无效") from exc
            prefix = settings["name_prefix"]
            settings["name"] = overrides.get("name") or (
                prefix + f"{settings['name_start'] + index:0{settings['name_width']}d}"
                if prefix else str(record.get("email", "")).strip()
            )
            if not settings["name"]:
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号缺少 email，无法按邮箱命名")
            prepared[(batch.index, index)] = settings
    managed_extra = sorted(
        {name for settings in prepared.values() for name in settings["extra"]}
        - {"imported_at", "access_token_sha256", OWNERSHIP_EXTRA_KEY}
    )
    account_snapshots: Dict[str, Dict[str, Any]] = {}
    managed_ids: Dict[str, int] = {}
    remote_accounts: List[Dict[str, Any]] = []
    remote_index: Dict[str, List[Dict[str, Any]]] = {}
    if not what_if:
        remote_accounts = get_accounts(client, token)
        remote_index = account_index(remote_accounts)
        for account in remote_accounts:
            current = snapshot(account, managed_extra)
            account_snapshots[str(current["id"])] = current

    created = modified = unchanged = skipped = manual_skipped = 0
    for batch in batches:
        for index, record in enumerate(batch.records):
            settings = prepared[(batch.index, index)]
            claim_existing_accounts = settings["claim_existing_accounts"]
            key = record_key(record)
            if key is None:
                raise Sub2ApiError(
                    f"第 {batch.index} 个批次第 {index + 1} 个账号缺少 email/account_id"
                )
            existing_account = (
                matching_account(remote_index, record) if not what_if else None
            )
            if existing_account is not None:
                existing_account = account_detail(
                    client, token, account_database_id(existing_account)
                )
                if not set(record_keys(record)).intersection(account_keys(existing_account)):
                    raise Sub2ApiError(
                        f"第 {batch.index} 个批次第 {index + 1} 个账号的远端标识已变化；已停止导入"
                    )
            if (
                existing_account is not None
                and not is_tool_managed(existing_account)
                and not claim_existing_accounts
            ):
                existing_id = account_database_id(existing_account)
                print(
                    f"跳过手动账户：{account_label(None, existing_id, str(record.get('email', '')), batch.index, index + 1)}"
                )
                manual_skipped += 1
                continue
            existing_id = account_database_id(existing_account) if existing_account else 0
            if (
                existing_account is not None
                and not is_tool_managed(existing_account)
                and claim_existing_accounts
            ):
                print(
                    f"认领已有账户：{account_label(None, existing_id, str(record.get('email', '')), batch.index, index + 1)}"
                )
            if existing_id > 0:
                managed_ids[key] = existing_id
            if key in batch.settings_only_keys and existing_id <= 0 and not what_if:
                raise Sub2ApiError("仅更新设置的账号在 Sub2API 中不存在；请先完成账户导入")
            name = settings["name"]
            expires_at = expires_timestamp(settings["expires_at"])
            update_payload = {
                "name": name,
                "notes": settings["notes"],
                "proxy_id": settings["proxy_id"] or 0,
                "concurrency": settings["concurrency"],
                "priority": settings["priority"],
                "rate_multiplier": settings["rate_multiplier"],
                "load_factor": settings["load_factor"] or 0,
                "group_ids": settings["group_ids"],
                # 更新接口用 0 清除可选值；省略或 null 会保留旧设置。
                "expires_at": expires_at or 0,
                "auto_pause_on_expired": settings["auto_pause_on_expired"],
                "extra": settings["extra"],
            }
            if what_if:
                print(
                    f"校验通过：{name}；分组={', '.join(settings['group_names']) or '无'}；"
                    f"代理={settings['proxy_name'] or '直连'}；并发={settings['concurrency']}；"
                    f"优先级={settings['priority']}；倍率={settings['rate_multiplier']}"
                )
                continue

            if key in batch.settings_only_keys:
                update_payload["extra"] = {**account_extra(existing_account), **settings["extra"]}
                account_snapshots[str(existing_id)] = snapshot(existing_account, managed_extra)
                client.request("PUT", f"/admin/accounts/{existing_id}", token=token, body=update_payload)
                result = {"failed": 0, "items": [{"action": "updated", "account_id": existing_id}]}
            else:
                payload = {
                    **update_payload,
                    "content": json.dumps(record, ensure_ascii=False, separators=(",", ":")),
                    "proxy_id": settings["proxy_id"],
                    "update_existing": existing_id > 0,
                }
                for field_name, value in (("load_factor", settings["load_factor"]), ("expires_at", expires_at)):
                    if value is None:
                        payload.pop(field_name)
                result = client.request(
                    "POST",
                    "/admin/accounts/import/codex-session",
                    token=token,
                    body=payload,
                )
            if not isinstance(result, dict):
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号返回格式无效")
            failed = result_count(result, "failed")
            if failed > 0:
                detail = failed_item_message(result, index + 1)
                detail_suffix = f"：{detail}" if detail else ""
                raise Sub2ApiError(
                    f"第 {batch.index} 个批次第 {index + 1} 个账号导入失败"
                    f"{detail_suffix}；凭据内容未输出"
                )
            items = result.get("items", [])
            if not isinstance(items, list):
                items = []
            item = items[0] if items and isinstance(items[0], dict) else {}
            action = str(item.get("action", ""))
            if not action:
                action = next(
                    (
                        candidate
                        for candidate, key in (
                            ("created", "created"),
                            ("updated", "updated"),
                            ("skipped", "skipped"),
                        )
                        if result_count(result, key) > 0
                    ),
                    "",
                )
            try:
                account_id = int(item.get("account_id", item.get("id", 0)) or 0)
            except (TypeError, ValueError):
                account_id = 0
            if account_id <= 0 and existing_id > 0:
                account_id = existing_id
            fallback_name = str(item.get("name", name))
            before = account_snapshots.get(str(account_id))
            after = None
            if account_id > 0 and action in ("created", "updated", "skipped"):
                after_value = account_detail(client, token, account_id)
                if not is_tool_managed(after_value):
                    raise Sub2ApiError(
                        f"账户 ID {account_id} 导入后未带工具归属标记；已停止写入增量映射"
                    )
                if key not in account_keys(after_value):
                    raise Sub2ApiError(
                        f"账户 ID {account_id} 导入后的账号标识不匹配；已停止写入增量映射"
                    )
                if existing_id > 0 and account_id != existing_id:
                    raise Sub2ApiError(
                        f"账户 ID {account_id} 与导入前确认的 ID {existing_id} 不一致；已停止写入增量映射"
                    )
                if key not in batch.settings_only_keys and action in ("created", "updated"):
                    # 凭据导入接口不会更新名称、备注或清空分组，再用设置接口统一行为。
                    update_payload["extra"] = {**account_extra(after_value), **settings["extra"]}
                    if not optional_string(record.get("refresh_token")):
                        # 保留上游为 accessToken-only 账户计算的到期和自动暂停策略。
                        update_payload.pop("expires_at")
                        update_payload.pop("auto_pause_on_expired")
                    client.request("PUT", f"/admin/accounts/{account_id}", token=token, body=update_payload)
                    after_value = account_detail(client, token, account_id)
                after = snapshot(after_value, managed_extra)
                managed_ids[key] = account_id
                account_snapshots[str(account_id)] = after
                remote_accounts = [
                    account
                    for account in remote_accounts
                    if account_database_id(account) != account_id
                ]
                remote_accounts.append(after_value)
                remote_index = account_index(remote_accounts)
            elif action in ("created", "updated"):
                raise Sub2ApiError(
                    f"第 {batch.index} 个批次第 {index + 1} 个账号导入成功但没有返回账户 ID"
                )
            label = account_label(after, account_id, fallback_name, batch.index, index + 1)
            if action == "created":
                created += 1
                print(f"新增账户：{label}")
            elif action == "updated":
                fields = changed_fields(before, after) if before is not None and after is not None else ["无法确定（缺少账户快照）"]
                if fields:
                    modified += 1
                    print(f"修改账户：{label}；字段：{'、'.join(fields)}")
                else:
                    unchanged += 1
            elif action == "skipped":
                skipped += 1
            else:
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号返回了无法识别的导入结果")

    if not what_if:
        skipped_total = skipped + manual_skipped
        suffix = f"，跳过 {skipped_total}" if skipped_total else ""
        print(f"导入完成：新增 {created}，修改 {modified}，无变化 {unchanged}{suffix}")
        if summary_file is not None:
            write_managed_summary(summary_file, managed_ids)
    return 0


def resolve_account_ids(
    config_file: Optional[Path],
    records: Iterable[Dict[str, Any]],
    *,
    require_managed: bool = True,
) -> Dict[str, int]:
    """只读查询输入账号对应的、可安全确认的 Sub2API 数据库 ID。"""
    record_list = list(records)
    wanted: Dict[str, str] = {}
    for record in record_list:
        canonical = record_key(record)
        if canonical is None:
            continue
        for key in record_keys(record):
            if key in wanted and wanted[key] != canonical:
                raise Sub2ApiError(f"账号标识 {key} 同时对应多个输入账号")
            wanted[key] = canonical
    if not wanted:
        return {}
    _, _, client, token = admin_session(config_file)
    result: Dict[str, int] = {}
    details: Dict[int, Dict[str, Any]] = {}
    for account in get_accounts(client, token):
        account_id = account_database_id(account)
        if account_id <= 0:
            continue
        for key in set(account_keys(account)) & wanted.keys():
            canonical = wanted[key]
            detail = details.get(account_id)
            if detail is None:
                detail = account_detail(client, token, account_id)
                details[account_id] = detail
            if require_managed and not is_tool_managed(detail):
                continue
            if key not in account_keys(detail):
                continue
            previous = result.get(canonical)
            if previous is not None and previous != account_id:
                raise Sub2ApiError(
                    f"账号 {canonical} 匹配到多个 Sub2API 账户；已停止以避免误匹配"
                )
            result[canonical] = account_id
    return result


def resolve_config(config_file: Optional[Path]) -> Path:
    if config_file is not None:
        path = config_file.expanduser().resolve()
    else:
        path = next(
            (candidate for candidate in (CANONICAL_CONFIG, COMPAT_CONFIG, DEFAULT_CONFIG) if candidate.is_file()),
            DEFAULT_CONFIG,
        )
    if not path.is_file():
        raise Sub2ApiError(f"找不到 Sub2API 导入配置：{path}")
    return path


def execute(
    input_file: Optional[Path] = None,
    config_file: Optional[Path] = None,
    *,
    cockpit_tools: bool = False,
    what_if: bool = False,
    summary_file: Optional[Path] = None,
) -> int:
    return run_import(
        input_file.expanduser().resolve() if input_file is not None else None,
        config_file,
        cockpit_tools=cockpit_tools,
        what_if=what_if,
        summary_file=summary_file,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description="把 Codex 账号 JSON 导入 Sub2API")
    parser.add_argument("input_file", nargs="?", type=Path)
    parser.add_argument("--config", type=Path)
    parser.add_argument("--cockpit-tools", action="store_true")
    parser.add_argument("--what-if", action="store_true")
    parser.add_argument("--summary-file", type=Path)
    args = parser.parse_args()
    try:
        return execute(
            args.input_file,
            args.config,
            cockpit_tools=args.cockpit_tools,
            what_if=args.what_if,
            summary_file=args.summary_file,
        )
    except (Sub2ApiError, OSError, ValueError, TypeError, KeyError) as exc:
        print(f"Sub2API 导入失败：{exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
