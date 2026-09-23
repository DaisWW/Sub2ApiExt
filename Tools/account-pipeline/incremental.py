"""计算账号增量，并保存不含凭据的本地快照。"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, Iterable, List, Mapping, Optional, Sequence, Tuple


class IncrementalError(RuntimeError):
    """增量快照或输入不一致。"""


@dataclass
class Delta:
    current: Dict[str, Dict[str, Any]]
    previous: Dict[str, Dict[str, Any]]
    added: List[Dict[str, Any]]
    removed: List[Dict[str, Any]]
    unchanged: int
    scope_changed: bool = False


def text(value: Any) -> str:
    return value.strip() if isinstance(value, str) else ""


def account_key(record: Mapping[str, Any]) -> str:
    email = text(record.get("email"))
    if email:
        return "email:" + email.casefold()
    account_id = text(record.get("account_id"))
    if account_id:
        return "id:" + account_id.casefold()
    raise IncrementalError("标准化账号缺少 email/account_id，无法建立增量标识")


def input_scope(codes_file: Optional[Path], accounts_file: Optional[Path]) -> Dict[str, str]:
    return {
        "codes_file": str(codes_file.resolve()) if codes_file is not None else "",
        "accounts_file": str(accounts_file.resolve()) if accounts_file is not None else "",
    }


def read_state(path: Path) -> Dict[str, Any]:
    if not path.is_file():
        return {"version": 2, "scope": None, "accounts": {}}
    try:
        value = json.loads(path.read_text(encoding="utf-8-sig"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise IncrementalError(f"增量快照无法读取：{path}") from exc
    if not isinstance(value, dict):
        raise IncrementalError(f"增量快照格式无效：{path}")
    if value.get("version") == 1:
        # v1 没有来源归属，不能安全地把其中的账号当成自动维护账号。
        return {"version": 2, "scope": None, "accounts": {}}
    if value.get("version") != 2:
        raise IncrementalError(f"增量快照版本无效：{path}")
    accounts = value.get("accounts", {})
    if not isinstance(accounts, dict):
        raise IncrementalError(f"增量快照 accounts 格式无效：{path}")
    return value


def _state_accounts(value: Mapping[str, Any]) -> Dict[str, Dict[str, Any]]:
    accounts = value.get("accounts", {})
    result: Dict[str, Dict[str, Any]] = {}
    if not isinstance(accounts, dict):
        return result
    for key, item in accounts.items():
        if not isinstance(key, str) or not isinstance(item, dict):
            continue
        try:
            sub2api_id = int(item.get("sub2api_id", 0) or 0)
        except (TypeError, ValueError):
            sub2api_id = 0
        sources = item.get("sources", [])
        if not isinstance(sources, list):
            sources = []
        result[key] = {
            "email": text(item.get("email")),
            "account_id": text(item.get("account_id")),
            "sub2api_id": sub2api_id if sub2api_id > 0 else None,
            "sources": sorted(
                value.strip()
                for value in sources
                if isinstance(value, str) and value.strip()
            ),
        }
    return result


def compare(
    records: Sequence[Dict[str, Any]],
    state: Mapping[str, Any],
    scope: Mapping[str, str],
) -> Delta:
    current: Dict[str, Dict[str, Any]] = {}
    for record in records:
        key = account_key(record)
        if key in current:
            raise IncrementalError(f"输入包含重复增量标识：{key}")
        current[key] = dict(record)

    stored_scope = state.get("scope")
    scope_changed = stored_scope not in (None, dict(scope))
    previous = {} if scope_changed else _state_accounts(state)
    added = [record for key, record in current.items() if key not in previous]
    removed = [record for key, record in previous.items() if key not in current]
    return Delta(
        current=current,
        previous=previous,
        added=added,
        removed=removed,
        unchanged=len(current.keys() & previous.keys()),
        scope_changed=scope_changed,
    )


def sub2api_payload(records: Iterable[Dict[str, Any]]) -> Dict[str, Any]:
    return {
        "type": "sub2api-data",
        "version": 1,
        "accounts": [
            {"name": text(record.get("email")), "credentials": record}
            for record in records
        ],
    }


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
        raise IncrementalError(f"写入增量文件失败：{path}") from exc


def write_delta(output_dir: Path, delta: Delta) -> Dict[str, str]:
    output_dir.mkdir(parents=True, exist_ok=True)
    sub2api_path = output_dir / "incremental-sub2api.json"
    cockpit_path = output_dir / "incremental-cockpit.json"
    removed_path = output_dir / "removed.json"
    write_json(sub2api_path, sub2api_payload(delta.added))
    write_json(cockpit_path, list(delta.added))
    write_json(removed_path, delta.removed)
    return {
        "sub2api_input": str(sub2api_path.resolve()),
        "cockpit_input": str(cockpit_path.resolve()),
        "removed": str(removed_path.resolve()),
    }


def build_state(
    records: Iterable[Dict[str, Any]],
    scope: Mapping[str, str],
    sub2api_ids: Mapping[str, Any],
    managed_keys: Optional[Iterable[str]] = None,
    source_keys: Optional[Mapping[str, Iterable[str]]] = None,
) -> Dict[str, Any]:
    managed_key_set = set(managed_keys) if managed_keys is not None else None
    accounts: Dict[str, Dict[str, Any]] = {}
    for record in records:
        key = account_key(record)
        if managed_key_set is not None and key not in managed_key_set:
            continue
        item: Dict[str, Any] = {
            "email": text(record.get("email")),
            "account_id": text(record.get("account_id")),
        }
        if source_keys is not None:
            values = source_keys.get(key, ())
            item["sources"] = sorted(
                value.strip()
                for value in values
                if isinstance(value, str) and value.strip()
            )
        remote_id = sub2api_ids.get(key)
        if remote_id is not None:
            try:
                item["sub2api_id"] = int(remote_id)
            except (TypeError, ValueError):
                pass
        accounts[key] = item
    return {"version": 2, "scope": dict(scope), "accounts": accounts}


def write_state(path: Path, state: Mapping[str, Any]) -> None:
    write_json(path, dict(state))


def update_pending_deletions(
    path: Path,
    removed: Iterable[Dict[str, Any]],
    current: Iterable[Dict[str, Any]],
) -> int:
    pending: Dict[str, str] = {}
    if path.is_file():
        try:
            for line in path.read_text(encoding="utf-8-sig").splitlines():
                value = line.strip()
                if value and not value.startswith("#"):
                    pending[value.casefold()] = value
        except (OSError, UnicodeDecodeError) as exc:
            raise IncrementalError(f"Cockpit 待手动处理清单无法读取：{path}") from exc
    for record in current:
        value = text(record.get("email")) or text(record.get("account_id"))
        if value:
            pending.pop(value.casefold(), None)
    for record in removed:
        value = text(record.get("email")) or text(record.get("account_id"))
        if value:
            pending[value.casefold()] = value
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    content = "# Cockpit 待手动处理账号；工具不会自动删除，请在 Cockpit Tools 中处理\n"
    content += "\n".join(pending.values())
    if pending:
        content += "\n"
    try:
        temporary.write_text(content, encoding="utf-8-sig")
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise IncrementalError(f"写入 Cockpit 待手动处理清单失败：{path}") from exc
    return len(pending)


def update_sub2api_pending_deletions(
    path: Path,
    entries: Iterable[Tuple[Mapping[str, Any], str]],
    current: Iterable[Mapping[str, Any]] = (),
) -> int:
    """维护 Sub2API 待手动处理清单，不包含凭据，也不调用删除接口。"""
    pending: Dict[str, str] = {}
    if path.is_file():
        try:
            for line in path.read_text(encoding="utf-8-sig").splitlines():
                value = line.strip()
                if not value or value.startswith("#"):
                    continue
                fields = {
                    part.split("=", 1)[0]: part.split("=", 1)[1]
                    for part in value.split("\t")
                    if "=" in part
                }
                account_id = text(fields.get("ID"))
                identity = text(fields.get("账号"))
                if account_id == "-":
                    account_id = ""
                if identity == "-":
                    identity = ""
                key = (
                    "id:" + account_id
                    if account_id
                    else "account:" + identity.casefold()
                )
                if key != "account:":
                    pending[key] = value
        except (OSError, UnicodeDecodeError) as exc:
            raise IncrementalError(f"Sub2API 待手动处理清单无法读取：{path}") from exc

    for record in current:
        identities = {
            "account:" + value.casefold()
            for value in (
                text(record.get("email")),
                text(record.get("account_id")),
            )
            if value
        }
        raw_account_id = record.get("sub2api_id")
        try:
            account_id = int(raw_account_id or 0)
        except (TypeError, ValueError):
            account_id = 0
        if account_id > 0:
            identities.add("id:" + str(account_id))
        for key in identities:
            pending.pop(key, None)

    for record, reason in entries:
        raw_account_id = record.get("sub2api_id")
        account_id = str(raw_account_id).strip() if raw_account_id is not None else ""
        identity = text(record.get("email")) or text(record.get("account_id"))
        if not account_id and not identity:
            continue
        clean_reason = str(reason).replace("\t", " ").replace("\r", " ").replace("\n", " ")
        line = (
            f"ID={account_id or '-'}\t账号={identity or '-'}\t原因={clean_reason}"
        )
        key = (
            "id:" + account_id
            if account_id
            else "account:" + identity.casefold()
        )
        pending[key] = line

    lines = ["# Sub2API 待手动处理账号；工具不会自动删除，请按 ID 或邮箱处理\n"]
    lines.extend(value + "\n" for value in pending.values())
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    try:
        temporary.write_text("".join(lines), encoding="utf-8-sig")
        os.replace(temporary, path)
    except OSError as exc:
        try:
            temporary.unlink(missing_ok=True)
        except OSError:
            pass
        raise IncrementalError(f"写入 Sub2API 待手动处理清单失败：{path}") from exc
    return len(pending)
