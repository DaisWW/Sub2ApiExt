"""比较并保存明文 token 快照。"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Iterable, Mapping, Sequence


TOKEN_FIELDS = ("access_token", "refresh_token", "id_token")


class TokenStateError(RuntimeError):
    """token 快照无法读取或写入。"""


@dataclass
class TokenDelta:
    changed: list[Dict[str, Any]]
    unchanged: int


def text(value: Any) -> str:
    return value.strip() if isinstance(value, str) else ""


def account_key(record: Mapping[str, Any]) -> str:
    email = text(record.get("email"))
    if email:
        return "email:" + email.casefold()
    account_id = text(record.get("account_id"))
    if account_id:
        return "id:" + account_id.casefold()
    raise TokenStateError("标准化账号缺少 email/account_id，无法比较 token")


def token_values(record: Mapping[str, Any]) -> Dict[str, str]:
    return {name: text(record.get(name)) for name in TOKEN_FIELDS}


def read_snapshot(path: Path) -> Dict[str, Any]:
    if not path.is_file():
        return {"version": 1, "accounts": {}}
    try:
        value = json.loads(path.read_text(encoding="utf-8-sig"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise TokenStateError(f"明文 token 快照无法读取：{path}") from exc
    if not isinstance(value, dict) or value.get("version") != 1:
        raise TokenStateError(f"明文 token 快照版本无效：{path}")
    accounts = value.get("accounts")
    if not isinstance(accounts, dict):
        raise TokenStateError(f"明文 token 快照 accounts 格式无效：{path}")
    return value


def compare(
    records: Sequence[Dict[str, Any]], snapshot: Mapping[str, Any]
) -> TokenDelta:
    raw_accounts = snapshot.get("accounts", {})
    accounts = raw_accounts if isinstance(raw_accounts, dict) else {}
    changed: list[Dict[str, Any]] = []
    unchanged = 0
    seen: set[str] = set()
    for record in records:
        key = account_key(record)
        if key in seen:
            raise TokenStateError(f"输入包含重复 token 标识：{key}")
        seen.add(key)
        previous = accounts.get(key)
        if isinstance(previous, dict) and token_values(previous) == token_values(record):
            unchanged += 1
        else:
            changed.append(record)
    return TokenDelta(changed=changed, unchanged=unchanged)


def update_snapshot(
    path: Path,
    records: Iterable[Mapping[str, Any]],
    sub2api_ids: Mapping[str, Any],
) -> None:
    snapshot = read_snapshot(path)
    raw_accounts = snapshot.get("accounts", {})
    accounts = dict(raw_accounts) if isinstance(raw_accounts, dict) else {}
    for record in records:
        key = account_key(record)
        item: Dict[str, Any] = {
            "email": text(record.get("email")),
            "account_id": text(record.get("account_id")),
            **token_values(record),
        }
        remote_id = sub2api_ids.get(key)
        if remote_id is not None:
            try:
                item["sub2api_id"] = int(remote_id)
            except (TypeError, ValueError):
                pass
        accounts[key] = item

    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    try:
        temporary.write_text(
            json.dumps({"version": 1, "accounts": accounts}, ensure_ascii=False, indent=2)
            + "\n",
            encoding="utf-8",
        )
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise TokenStateError(f"写入明文 token 快照失败：{path}") from exc
