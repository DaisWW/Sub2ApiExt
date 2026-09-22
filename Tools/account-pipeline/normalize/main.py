"""把兑换站下载的 JSON 统一成 Sub2API 和 Cockpit 可用的输入。"""

from __future__ import annotations

import argparse
import base64
import binascii
import json
import os
import sys
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Set


MAX_JSON_BYTES = 32 * 1024 * 1024
MAX_SOURCE_FILES = 1000
MAX_ACCOUNTS = 1000
TOKEN_KEYS = ("access_token", "accessToken")
EMAIL_KEYS = ("email", "user_email", "account_email", "accountEmail", "username")
REFRESH_KEYS = ("refresh_token", "refreshToken")
ID_TOKEN_KEYS = ("id_token", "idToken")
ACCOUNT_ID_KEYS = ("account_id", "accountId", "chatgpt_account_id")
SUPPORTED_TYPES = {"codex"}


class NormalizeError(RuntimeError):
    """下载包格式或内容无效。"""


def text_value(value: Any) -> Optional[str]:
    if isinstance(value, str) and value.strip():
        return value.strip()
    return None


def pick_string(obj: Dict[str, Any], keys: Iterable[str]) -> Optional[str]:
    for key in keys:
        value = text_value(obj.get(key))
        if value:
            return value
    return None


def has_token(obj: Dict[str, Any]) -> bool:
    return pick_string(obj, TOKEN_KEYS) is not None


def merge_record(parent: Dict[str, Any], child: Dict[str, Any]) -> Dict[str, Any]:
    merged = dict(parent)
    merged.update(child)
    return merged


def candidate_records(value: Any) -> Iterable[Dict[str, Any]]:
    """只沿常见账号容器递归，避免把任意嵌套 JSON 当成账号。"""
    if isinstance(value, list):
        for item in value:
            yield from candidate_records(item)
        return
    if not isinstance(value, dict):
        return

    if has_token(value):
        yield value
        return

    credentials = value.get("credentials")
    if isinstance(credentials, dict):
        merged = merge_record(value, credentials)
        if has_token(merged):
            yield merged
            return

    accounts = value.get("accounts")
    if isinstance(accounts, list):
        for item in accounts:
            if isinstance(item, dict) and isinstance(item.get("credentials"), dict):
                merged = merge_record(item, item["credentials"])
                if has_token(merged):
                    yield merged
            else:
                yield from candidate_records(item)
        return

    for key in ("data", "items", "records", "result", "auth", "tokens"):
        nested = value.get(key)
        if isinstance(nested, (dict, list)):
            if isinstance(nested, dict):
                merged = merge_record(value, nested)
                if has_token(merged):
                    yield merged
                    continue
            yield from candidate_records(nested)


def read_json(path: Path) -> Any:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise NormalizeError(f"读取下载文件失败：{path}") from exc
    if not raw:
        raise NormalizeError(f"下载文件为空：{path}")
    if len(raw) > MAX_JSON_BYTES:
        raise NormalizeError(f"下载文件超过 {MAX_JSON_BYTES // (1024 * 1024)} MiB：{path}")
    try:
        if path.suffix.lower() == ".jsonl":
            return [json.loads(line) for line in raw.decode("utf-8-sig").splitlines() if line.strip()]
        return json.loads(raw.decode("utf-8-sig"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise NormalizeError(f"下载文件不是有效 UTF-8 JSON：{path}") from exc


def source_files(data_dir: Path) -> List[Path]:
    if not data_dir.is_dir():
        raise NormalizeError(f"找不到解压目录：{data_dir}")
    files = sorted(
        path
        for path in data_dir.rglob("*")
        if path.is_file() and path.suffix.lower() in (".json", ".jsonl")
    )
    if len(files) > MAX_SOURCE_FILES:
        raise NormalizeError(f"下载包中的 JSON 文件超过 {MAX_SOURCE_FILES} 个")
    if not files:
        raise NormalizeError(f"解压目录中没有 JSON 文件：{data_dir}")
    return files


def decode_jwt_claims(token: Optional[str]) -> Dict[str, Any]:
    if not token:
        return {}
    parts = token.split(".")
    if len(parts) != 3:
        return {}
    try:
        padding = "=" * (-len(parts[1]) % 4)
        payload = base64.urlsafe_b64decode((parts[1] + padding).encode("ascii"))
        value = json.loads(payload.decode("utf-8"))
    except (ValueError, UnicodeError, binascii.Error, json.JSONDecodeError):
        return {}
    return value if isinstance(value, dict) else {}


def nested_claim(claims: Dict[str, Any], keys: Iterable[str]) -> Optional[str]:
    wanted = {key.lower() for key in keys}
    stack: List[Any] = [claims]
    while stack:
        current = stack.pop()
        if isinstance(current, dict):
            for key, value in current.items():
                if str(key).lower() in wanted:
                    found = text_value(value)
                    if found:
                        return found
                if isinstance(value, (dict, list)):
                    stack.append(value)
        elif isinstance(current, list):
            stack.extend(current)
    return None


def first_claim(
    sources: Iterable[Dict[str, Any]],
    direct_keys: Iterable[str],
    nested_keys: Iterable[str] = (),
) -> Optional[str]:
    """按来源顺序查找直接字段，再查找常见嵌套 claim。"""
    source_list = tuple(sources)
    for source in source_list:
        value = pick_string(source, direct_keys)
        if value:
            return value
    for source in source_list:
        value = nested_claim(source, nested_keys)
        if value:
            return value
    return None


def canonical_record(record: Dict[str, Any], number: int) -> Dict[str, Any]:
    record_type = text_value(record.get("type"))
    if record_type and record_type.lower() not in SUPPORTED_TYPES:
        raise NormalizeError(f"第 {number} 个账号类型不受支持：{record_type}")

    access_token = pick_string(record, TOKEN_KEYS)
    if not access_token:
        raise NormalizeError(f"第 {number} 个账号缺少 access_token")

    claims = decode_jwt_claims(access_token)
    id_token = pick_string(record, ID_TOKEN_KEYS)
    id_claims = decode_jwt_claims(id_token)
    claim_sets = (record, claims, id_claims)

    email = first_claim(
        claim_sets,
        EMAIL_KEYS,
        ("email", "user_email", "preferred_username"),
    )
    if not email:
        raise NormalizeError(f"第 {number} 个账号缺少 email，无法同时导入两个目标")

    account_id = first_claim(claim_sets, ACCOUNT_ID_KEYS, ACCOUNT_ID_KEYS)

    refresh_token = pick_string(record, REFRESH_KEYS)
    if not refresh_token:
        refresh_token = pick_string(claims, REFRESH_KEYS)

    normalized: Dict[str, Any] = {
        "type": "codex",
        "email": email,
        "access_token": access_token,
    }
    if refresh_token:
        normalized["refresh_token"] = refresh_token
    if id_token:
        normalized["id_token"] = id_token
    if account_id:
        normalized["account_id"] = account_id
    expires_at = record.get("expires_at")
    if isinstance(expires_at, (str, int, float)) and not isinstance(expires_at, bool):
        normalized["expires_at"] = expires_at
    return normalized


def deduplicate(records: Iterable[Dict[str, Any]]) -> List[Dict[str, Any]]:
    result: List[Dict[str, Any]] = []
    seen: Set[str] = set()
    for record in records:
        account_id = text_value(record.get("account_id"))
        email = text_value(record.get("email"))
        if account_id:
            key = "id:" + account_id.lower()
        else:
            key = "email:" + email.lower()
        if key in seen:
            continue
        seen.add(key)
        result.append(record)
    return result


def write_json(path: Path, value: Any) -> None:
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
        raise NormalizeError(f"写入标准化文件失败：{path}") from exc


def normalize(data_dir: Path, output_dir: Path) -> Dict[str, Any]:
    files = source_files(data_dir)
    raw_records: List[Dict[str, Any]] = []
    for path in files:
        value = read_json(path)
        raw_records.extend(candidate_records(value))
    if not raw_records:
        raise NormalizeError("下载包中没有找到包含 access_token 的 Codex 账号")
    if len(raw_records) > MAX_ACCOUNTS:
        raise NormalizeError(f"账号数量超过 {MAX_ACCOUNTS} 个")

    records = deduplicate(
        canonical_record(record, index)
        for index, record in enumerate(raw_records, start=1)
    )
    if not records:
        raise NormalizeError("标准化后没有可导入账号")

    sub2api_accounts = [
        {"name": record["email"], "credentials": record} for record in records
    ]
    sub2api_payload = {
        "type": "sub2api-data",
        "version": 1,
        "accounts": sub2api_accounts,
    }
    cockpit_payload = records
    sub2api_path = output_dir / "sub2api-accounts.json"
    cockpit_path = output_dir / "cockpit-accounts.json"
    write_json(sub2api_path, sub2api_payload)
    write_json(cockpit_path, cockpit_payload)
    metadata = {
        "version": 1,
        "source_files": len(files),
        "accounts": len(records),
        "sub2api_input": str(sub2api_path.resolve()),
        "cockpit_input": str(cockpit_path.resolve()),
    }
    write_json(output_dir / "manifest.json", metadata)
    return metadata


def main() -> int:
    parser = argparse.ArgumentParser(description="标准化下载的账号 JSON")
    parser.add_argument("--data-dir", required=True, type=Path)
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    try:
        metadata = normalize(args.data_dir, args.output_dir)
    except (NormalizeError, OSError, ValueError, TypeError, KeyError) as exc:
        print(f"标准化模块失败：{exc}", file=sys.stderr)
        return 1
    print(f"已标准化 {metadata['accounts']} 个账号")
    print(f"Sub2API 输入：{metadata['sub2api_input']}")
    print(f"Cockpit 输入：{metadata['cockpit_input']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
