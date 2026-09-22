"""使用 Sub2API Admin API 导入或更新 Codex 账号。"""

from __future__ import annotations

import argparse
import copy
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
from typing import Any, Dict, Iterable, List, Optional, Sequence

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
    group_ids: List[int] = field(default_factory=list)


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
        return json.loads(raw.decode("utf-8-sig"))
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


def required_string(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise Sub2ApiError(f"{label}缺失")
    return value.strip()


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


def read_records(path: Path, source_type: str) -> List[Dict[str, Any]]:
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
            batches.append(
                Batch(number, raw_batch, path, str(path), batch_source, groups, read_records(path, batch_source))
            )
        return batches

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
        records = read_records(path, source_type)
        label = str(path)
    groups = as_string_list(config.get("group_names", []), "group_names")
    return [Batch(1, None, path, label, source_type, groups, records)]


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


def record_key(record: Dict[str, Any]) -> Optional[str]:
    email = optional_string(record.get("email"))
    if email:
        return "email:" + email.casefold()
    account_id = optional_string(record.get("account_id"))
    return "id:" + account_id.casefold() if account_id else None


def account_emails(account: Dict[str, Any]) -> List[str]:
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


def write_managed_summary(path: Path, account_ids: Dict[str, int]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    try:
        temporary.write_text(
            json.dumps({"version": 1, "accounts": account_ids}, ensure_ascii=False, indent=2)
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


def comparable(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def snapshot(account: Dict[str, Any], managed_extra: Iterable[str]) -> Dict[str, Any]:
    extra = property_value(account, "extra", {})
    if not isinstance(extra, dict):
        extra = {}
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
    config_dir = config_path.parent
    batches = build_batches(config_value, config_dir, input_file, cockpit_tools)

    groups_value = client.request("GET", "/admin/groups/all?include_inactive=true", token=token)
    groups = groups_value if isinstance(groups_value, list) else []
    if not isinstance(groups_value, list):
        raise Sub2ApiError("Sub2API 分组列表格式无效")
    group_summaries = []
    for batch in batches:
        for group_name in batch.group_names:
            group = resolve_unique(groups, group_name, "分组")
            if group.get("platform") != "openai" or group.get("status") != "active":
                raise Sub2ApiError(f"第 {batch.index} 个批次的分组 '{group_name}' 必须是启用状态的 OpenAI 分组")
            try:
                batch.group_ids.append(int(group["id"]))
            except (KeyError, TypeError, ValueError) as exc:
                raise Sub2ApiError(f"分组 '{group_name}' 的 ID 无效") from exc
        group_summaries.append(", ".join(batch.group_names) if batch.group_names else "无")

    proxy_id = None
    proxy_name = str(config_value.get("proxy_name", "") or "").strip()
    if proxy_name:
        proxies_value = client.request("GET", "/admin/proxies/all", token=token)
        if not isinstance(proxies_value, list):
            raise Sub2ApiError("Sub2API 代理列表格式无效")
        proxy = resolve_unique(proxies_value, proxy_name, "代理")
        if proxy.get("status") != "active":
            raise Sub2ApiError(f"代理 '{proxy_name}' 当前未启用")
        try:
            proxy_id = int(proxy["id"])
        except (KeyError, TypeError, ValueError) as exc:
            raise Sub2ApiError(f"代理 '{proxy_name}' 的 ID 无效") from exc

    concurrency = integer_value(config_value.get("concurrency", 3), "concurrency")
    priority = integer_value(config_value.get("priority", 50), "priority")
    rate_multiplier = number_value(
        config_value.get("rate_multiplier", 1), "rate_multiplier"
    )
    if concurrency < 1:
        raise Sub2ApiError("concurrency 必须大于 0")
    if priority < 1:
        raise Sub2ApiError("priority 必须大于 0")
    if rate_multiplier < 0:
        raise Sub2ApiError("rate_multiplier 不能小于 0")
    load_factor = config_value.get("load_factor")
    if load_factor is not None:
        if isinstance(load_factor, bool):
            raise Sub2ApiError("load_factor 必须大于 0，或设为 null 使用默认值")
        load_factor = integer_value(load_factor, "load_factor")
        if load_factor < 1:
            raise Sub2ApiError("load_factor 必须大于 0，或设为 null 使用默认值")
    extra = config_value.get("extra") or {}
    if not isinstance(extra, dict):
        raise Sub2ApiError("extra 必须是 JSON 对象")
    managed_extra = sorted(
        name for name in extra
        if name not in ("imported_at", "access_token_sha256")
    )
    account_snapshots: Dict[str, Dict[str, Any]] = {}
    managed_ids: Dict[str, int] = {}
    if not what_if:
        for account in get_accounts(client, token):
            current = snapshot(account, managed_extra)
            account_snapshots[str(current["id"])] = current
            if current["id"] > 0:
                for email in account_emails(account):
                    managed_ids["email:" + email.casefold()] = current["id"]

    created = modified = unchanged = skipped = 0
    what_if_summaries = []
    expires_at = expires_timestamp(config_value.get("expires_at", ""))
    for batch, group_summary in zip(batches, group_summaries):
        name_prefix = str(effective(batch.config, config_value, "name_prefix", "") or "")
        name_start = integer_value(
            effective(batch.config, config_value, "name_start", 1),
            f"第 {batch.index} 个批次的 name_start",
        )
        name_width = integer_value(
            effective(batch.config, config_value, "name_width", 3),
            f"第 {batch.index} 个批次的 name_width",
        )
        if name_start < 0:
            raise Sub2ApiError(f"第 {batch.index} 个批次的 name_start 不能小于 0")
        if not 1 <= name_width <= 99:
            raise Sub2ApiError(f"第 {batch.index} 个批次的 name_width 必须在 1 到 99 之间")
        for index, record in enumerate(batch.records):
            if name_prefix:
                name = name_prefix + f"{name_start + index:0{name_width}d}"
            else:
                name = str(record.get("email", "")).strip()
                if not name:
                    raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号缺少 email，无法按邮箱命名")
            payload = {
                "content": json.dumps(record, ensure_ascii=False, separators=(",", ":")),
                "name": name,
                "notes": str(config_value.get("notes", "") or ""),
                "proxy_id": proxy_id,
                "concurrency": concurrency,
                "priority": priority,
                "rate_multiplier": rate_multiplier,
                "group_ids": batch.group_ids,
                "auto_pause_on_expired": bool(config_value.get("auto_pause_on_expired", False)),
                "extra": extra,
                "update_existing": True,
            }
            if load_factor is not None:
                payload["load_factor"] = load_factor
            if expires_at is not None:
                payload["expires_at"] = expires_at
            if what_if:
                continue

            result = client.request(
                "POST",
                "/admin/accounts/import/codex-session",
                token=token,
                body=payload,
            )
            if not isinstance(result, dict):
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号返回格式无效")
            try:
                failed = int(result.get("failed", 0))
            except (TypeError, ValueError) as exc:
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号返回格式无效") from exc
            if failed > 0:
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号导入失败；凭据内容未输出")
            items = result.get("items", [])
            if not isinstance(items, list):
                items = []
            item = items[0] if items and isinstance(items[0], dict) else {}
            action = str(item.get("action", ""))
            if not action:
                action = next(
                    (candidate for candidate, key in (("created", "created"), ("updated", "updated"), ("skipped", "skipped")) if int(result.get(key, 0) or 0) > 0),
                    "",
                )
            try:
                account_id = int(item.get("account_id", 0) or 0)
            except (TypeError, ValueError):
                account_id = 0
            key = record_key(record)
            if key is not None and account_id > 0:
                managed_ids[key] = account_id
            fallback_name = str(item.get("name", name))
            before = account_snapshots.get(str(account_id))
            after = None
            if account_id > 0 and action in ("created", "updated"):
                try:
                    after = snapshot(client.request("GET", f"/admin/accounts/{account_id}", token=token), managed_extra)
                except Sub2ApiError as exc:
                    print(f"警告：账户 ID {account_id} 已导入，但无法读取导入后的账户明细：{exc}", file=sys.stderr)
            label = account_label(after, account_id, fallback_name, batch.index, index + 1)
            if action == "created":
                created += 1
                if after is not None:
                    account_snapshots[str(account_id)] = after
                print(f"新增账户：{label}")
            elif action == "updated":
                fields = changed_fields(before, after) if before is not None and after is not None else ["无法确定（缺少账户快照）"]
                if fields:
                    modified += 1
                    print(f"修改账户：{label}；字段：{'、'.join(fields)}")
                else:
                    unchanged += 1
                if after is not None:
                    account_snapshots[str(account_id)] = after
            elif action == "skipped":
                skipped += 1
            else:
                raise Sub2ApiError(f"第 {batch.index} 个批次第 {index + 1} 个账号返回了无法识别的导入结果")

        if what_if:
            mode = f"前缀 {name_prefix}" if name_prefix else "邮箱"
            what_if_summaries.append(
                f"批次 {batch.index}：{len(batch.records)} 个账号；来源={batch.input_label}；命名={mode}；分组={group_summary}"
            )

    if what_if:
        proxy_summary = proxy_name or "直连"
        for summary in what_if_summaries:
            print(f"校验通过：{summary}；代理={proxy_summary}；并发={concurrency}；优先级={priority}；倍率={rate_multiplier}")
    else:
        suffix = f"，跳过 {skipped}" if skipped else ""
        print(f"导入完成：新增 {created}，修改 {modified}，无变化 {unchanged}{suffix}")
        if summary_file is not None:
            write_managed_summary(summary_file, managed_ids)
    return 0


def delete_account_ids(
    config_file: Optional[Path],
    account_ids: Iterable[int],
) -> int:
    """删除增量快照明确记录的 Sub2API 账户。"""
    ids = set()
    for value in account_ids:
        try:
            account_id = int(value)
        except (TypeError, ValueError):
            continue
        if account_id > 0:
            ids.add(account_id)
    ids = sorted(ids)
    if not ids:
        print("Sub2API：没有需要删除的账户。")
        return 0

    _, _, client, token = admin_session(config_file)
    result = client.request(
        "POST",
        "/admin/accounts/batch-delete",
        token=token,
        body={"account_ids": ids},
    )
    if not isinstance(result, dict):
        raise Sub2ApiError("Sub2API 删除接口返回格式无效")
    try:
        failed = int(result.get("failed", 0) or 0)
        success = int(result.get("success", 0) or 0)
    except (TypeError, ValueError) as exc:
        raise Sub2ApiError("Sub2API 删除接口返回格式无效") from exc
    if failed:
        failed_ids = result.get("failed_ids", [])
        detail = ", ".join(str(value) for value in failed_ids) if isinstance(failed_ids, list) else "未知账户"
        raise Sub2ApiError(f"Sub2API 删除失败 {failed} 个账户（ID：{detail}）")
    if success != len(ids):
        raise Sub2ApiError(
            f"Sub2API 删除结果数量不一致：请求 {len(ids)} 个，成功 {success} 个"
        )
    print(f"Sub2API 删除完成：{success} 个账户。")
    return 0


def resolve_account_ids(
    config_file: Optional[Path],
    records: Iterable[Dict[str, Any]],
    *,
    require_stored_id: bool = False,
) -> Dict[str, int]:
    """只读查询输入账号对应的 Sub2API 数据库 ID。"""
    record_list = list(records)
    wanted: Dict[str, Optional[int]] = {}
    for record in record_list:
        key = record_key(record)
        if key is None:
            continue
        try:
            remote_id = int(record.get("sub2api_id", 0) or 0)
        except (TypeError, ValueError):
            remote_id = 0
        if require_stored_id and remote_id <= 0:
            continue
        wanted[key] = remote_id if remote_id > 0 else None
    if not wanted:
        return {}
    _, _, client, token = admin_session(config_file)
    result: Dict[str, int] = {}
    for account in get_accounts(client, token):
        try:
            account_id = int(account.get("id", 0) or 0)
        except (TypeError, ValueError):
            continue
        if account_id <= 0:
            continue
        account_keys = {"email:" + email.casefold() for email in account_emails(account)}
        for key in account_keys & wanted.keys():
            expected_id = wanted[key]
            if expected_id is None or expected_id == account_id:
                result[key] = account_id
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


def validate_config(config_file: Optional[Path] = None) -> Path:
    path = resolve_config(config_file)
    value = json_file(path, "Sub2API 导入配置")
    if not isinstance(value, dict):
        raise Sub2ApiError("Sub2API 导入配置必须是 JSON 对象")
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
