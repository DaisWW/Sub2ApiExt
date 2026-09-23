"""把兑换站下载的 JSON 统一成 Sub2API 和 Cockpit 可用的输入。"""

from __future__ import annotations

import argparse
import base64
import binascii
import json
import os
import sys
from pathlib import Path
from typing import Any, Dict, Iterable, List, Optional, Set, Tuple


MAX_JSON_BYTES = 32 * 1024 * 1024
MAX_SOURCE_FILES = 1000
MAX_ACCOUNTS = 1000
TOKEN_KEYS = ("access_token", "accessToken")
EMAIL_KEYS = ("email", "user_email", "account_email", "accountEmail", "username")
REFRESH_KEYS = ("refresh_token", "refreshToken")
ID_TOKEN_KEYS = ("id_token", "idToken")
ACCOUNT_ID_KEYS = ("account_id", "accountId", "chatgpt_account_id")
SUPPORTED_TYPES = {"codex", "oauth"}
SUPPORTED_PLATFORMS = {"openai", "codex"}


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

    credentials = value.get("credentials")
    if isinstance(credentials, dict):
        merged = merge_record(value, credentials)
        if has_token(merged):
            yield merged
            return

    if has_token(value):
        yield value
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


def parse_documents(text: str, label: str) -> List[Any]:
    """读取一个或多个连续 JSON 文档，文档之间允许空行。"""
    decoder = json.JSONDecoder()
    values: List[Any] = []
    offset = 0
    length = len(text)
    while offset < length:
        while offset < length and text[offset].isspace():
            offset += 1
        if offset >= length:
            break
        try:
            value, next_offset = decoder.raw_decode(text, offset)
        except json.JSONDecodeError as exc:
            line = text.count("\n", 0, exc.pos) + 1
            raise NormalizeError(f"{label}第 {line} 行附近不是完整 JSON") from exc
        values.append(value)
        offset = next_offset
    return values


def read_json(
    path: Path, label: str = "JSON 文件", *, allow_empty: bool = False
) -> List[Any]:
    try:
        raw = path.read_bytes()
    except OSError as exc:
        raise NormalizeError(f"读取{label}失败：{path}") from exc
    if not raw and not allow_empty:
        raise NormalizeError(f"{label}为空：{path}")
    if len(raw) > MAX_JSON_BYTES:
        raise NormalizeError(
            f"{label}超过 {MAX_JSON_BYTES // (1024 * 1024)} MiB：{path}"
        )
    try:
        text = raw.decode("utf-8-sig")
    except UnicodeDecodeError as exc:
        raise NormalizeError(f"{label}不是有效 UTF-8 JSON：{path}") from exc
    if not text.strip() and allow_empty:
        return []
    return parse_documents(text, f"{label} {path} ")


def source_files(data_dir: Path, *, allow_empty: bool = False) -> List[Path]:
    if not data_dir.is_dir():
        raise NormalizeError(f"找不到解压目录：{data_dir}")
    files = sorted(
        path
        for path in data_dir.rglob("*")
        if path.is_file() and path.suffix.lower() in (".json", ".jsonl")
    )
    if len(files) > MAX_SOURCE_FILES:
        raise NormalizeError(f"下载包中的 JSON 文件超过 {MAX_SOURCE_FILES} 个")
    if not files and not allow_empty:
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
    if record_type and record_type.lower() == "oauth":
        platform = text_value(record.get("platform"))
        if platform and platform.lower() not in SUPPORTED_PLATFORMS:
            raise NormalizeError(f"第 {number} 个账号平台不受支持：{platform}")

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


def record_key(record: Dict[str, Any]) -> str:
    email = text_value(record.get("email"))
    if email:
        return "email:" + email.casefold()
    account_id = text_value(record.get("account_id"))
    if account_id:
        return "id:" + account_id.casefold()
    raise NormalizeError("标准化账号缺少 email/account_id，无法建立来源标识")


def deduplicate_with_sources(
    records: Iterable[Tuple[Dict[str, Any], str]],
) -> Tuple[List[Dict[str, Any]], Dict[str, Set[str]]]:
    result: List[Dict[str, Any]] = []
    seen: Set[str] = set()
    sources: Dict[str, Set[str]] = {}
    for record, source in records:
        key = record_key(record)
        sources.setdefault(key, set()).add(source)
        if key in seen:
            continue
        seen.add(key)
        result.append(record)
    return result, sources


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


def records_from_values(values: Iterable[Any]) -> List[Dict[str, Any]]:
    records: List[Dict[str, Any]] = []
    for value in values:
        records.extend(candidate_records(value))
    return records


def validate_input_file(path: Path, *, allow_empty: bool = False) -> None:
    """校验独立账户文本，不访问网络也不写输出。"""
    values = read_json(
        path.expanduser().resolve(), "账户文本", allow_empty=allow_empty
    )
    raw_records = records_from_values(values)
    if not raw_records and not allow_empty:
        raise NormalizeError(f"账户文本中没有找到包含 access_token 的账号：{path}")
    if len(raw_records) > MAX_ACCOUNTS:
        raise NormalizeError(f"账户文本中的账号数量超过 {MAX_ACCOUNTS} 个")
    for index, record in enumerate(raw_records, start=1):
        canonical_record(record, index)


def normalize(
    data_dir: Optional[Path],
    output_dir: Path,
    account_file: Optional[Path] = None,
    *,
    allow_empty: bool = False,
) -> Dict[str, Any]:
    files = (
        source_files(data_dir, allow_empty=account_file is not None)
        if data_dir is not None
        else []
    )
    if not files and account_file is None and not allow_empty:
        raise NormalizeError("没有提供兑换解压目录或独立账户文本")
    source_records: List[Tuple[Dict[str, Any], str]] = []
    for path in files:
        source_records.extend(
            (record, "redeem")
            for record in records_from_values(read_json(path, "下载文件"))
        )
    account_path: Optional[Path] = None
    if account_file is not None:
        account_path = account_file.expanduser().resolve()
        source_records.extend(
            (record, "accounts-file")
            for record in records_from_values(
                read_json(account_path, "账户文本", allow_empty=True)
            )
        )
    if not source_records and not allow_empty:
        raise NormalizeError("输入中没有找到包含 access_token 的 Codex 账号")
    if len(source_records) > MAX_ACCOUNTS:
        raise NormalizeError(f"账号数量超过 {MAX_ACCOUNTS} 个")

    canonical_sources = (
        (canonical_record(record, index), source)
        for index, (record, source) in enumerate(source_records, start=1)
    )
    records, sources = deduplicate_with_sources(canonical_sources)
    if not records and not allow_empty:
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
        "source_counts": {
            "redeem": sum("redeem" in values for values in sources.values()),
            "accounts-file": sum(
                "accounts-file" in values for values in sources.values()
            ),
            "overlap": sum(len(values) > 1 for values in sources.values()),
        },
        "sub2api_input": str(sub2api_path.resolve()),
        "cockpit_input": str(cockpit_path.resolve()),
        "source_keys": {
            key: sorted(values) for key, values in sorted(sources.items())
        },
    }
    if account_path is not None:
        metadata["account_file"] = str(account_path)
    write_json(output_dir / "manifest.json", metadata)
    return metadata


def main() -> int:
    parser = argparse.ArgumentParser(description="标准化下载的账号 JSON 或账户文本")
    parser.add_argument("--data-dir", type=Path, help="兑换 ZIP 的解压目录")
    parser.add_argument("--input-file", type=Path, help="可含多个 JSON 对象的账户文本")
    parser.add_argument("--output-dir", required=True, type=Path)
    args = parser.parse_args()
    try:
        if args.data_dir is None and args.input_file is None:
            parser.error("--data-dir 和 --input-file 至少提供一个")
        metadata = normalize(args.data_dir, args.output_dir, args.input_file)
    except (NormalizeError, OSError, ValueError, TypeError, KeyError) as exc:
        print(f"标准化模块失败：{exc}", file=sys.stderr)
        return 1
    counts = metadata["source_counts"]
    print(
        "输入合并："
        f"兑换来源 {counts['redeem']} 个，"
        f"账户文本 {counts['accounts-file']} 个，"
        f"重复 {counts['overlap']} 个，"
        f"合并后 {metadata['accounts']} 个。"
    )
    print(f"Sub2API 输入：{metadata['sub2api_input']}")
    print(f"Cockpit 输入：{metadata['cockpit_input']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
