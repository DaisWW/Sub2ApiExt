import base64
import json
import re
import sys
from pathlib import Path


class SourceError(Exception):
    pass


def read_json(path: Path, label: str):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as error:
        raise SourceError(f"无法读取 {label}") from error


def decode_base64(value, label: str) -> bytes:
    if not isinstance(value, str):
        raise SourceError(f"{label}格式无效")
    try:
        return base64.b64decode(value.strip(), validate=True)
    except (ValueError, TypeError) as error:
        raise SourceError(f"{label}格式无效") from error


def required_string(value, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise SourceError(f"{label}缺失")
    return value.strip()


def optional_string(value):
    return value.strip() if isinstance(value, str) and value.strip() else None


def load_records():
    try:
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    except ImportError as error:
        raise SourceError("导入 Cockpit Tools 账号需要 Python cryptography 包") from error

    data_directory = Path.home() / ".antigravity_cockpit"
    index_path = data_directory / "codex_accounts.json"
    accounts_directory = data_directory / "codex_accounts"
    key_path = data_directory / "secure-account-storage.key"

    index = read_json(index_path, "Cockpit Tools Codex 账号索引")
    summaries = index.get("accounts") if isinstance(index, dict) else None
    if not isinstance(summaries, list) or not summaries:
        raise SourceError("Cockpit Tools 中没有 Codex 账号")
    if not accounts_directory.is_dir():
        raise SourceError("找不到 Cockpit Tools Codex 账号目录")

    try:
        key = decode_base64(key_path.read_text(encoding="utf-8"), "Cockpit Tools 账号详情加密密钥")
    except OSError as error:
        raise SourceError("无法读取 Cockpit Tools 账号详情加密密钥") from error
    if len(key) != 32:
        raise SourceError("Cockpit Tools 账号详情加密密钥长度无效")

    decryptor = AESGCM(key)
    records = []
    seen_ids = set()
    for index_number, summary in enumerate(summaries, start=1):
        if not isinstance(summary, dict):
            raise SourceError(f"Cockpit Tools 第 {index_number} 个 Codex 账号索引无效")
        account_id = required_string(
            summary.get("id"),
            f"Cockpit Tools 第 {index_number} 个 Codex 账号 ID",
        )
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]*", account_id):
            raise SourceError(f"Cockpit Tools 第 {index_number} 个 Codex 账号 ID 无效")
        if account_id in seen_ids:
            raise SourceError("Cockpit Tools Codex 账号索引包含重复 ID")
        seen_ids.add(account_id)

        account_path = accounts_directory / f"{account_id}.json"
        envelope = read_json(account_path, f"Cockpit Tools 第 {index_number} 个 Codex 账号详情")
        if (
            not isinstance(envelope, dict)
            or envelope.get("version") != 1
            or envelope.get("kind") != "codex"
            or envelope.get("algorithm") != "AES-256-GCM"
        ):
            raise SourceError(f"Cockpit Tools 第 {index_number} 个 Codex 账号详情格式无效")

        nonce = decode_base64(
            envelope.get("nonce"),
            f"Cockpit Tools 第 {index_number} 个 Codex 账号 nonce",
        )
        ciphertext = decode_base64(
            envelope.get("ciphertext"),
            f"Cockpit Tools 第 {index_number} 个 Codex 账号密文",
        )
        if len(nonce) != 12 or len(ciphertext) <= 16:
            raise SourceError(f"Cockpit Tools 第 {index_number} 个 Codex 账号详情格式无效")

        try:
            account = json.loads(decryptor.decrypt(nonce, ciphertext, None))
        except Exception as error:
            raise SourceError(f"无法解密 Cockpit Tools 第 {index_number} 个 Codex 账号详情") from error

        if not isinstance(account, dict) or account.get("id") != account_id:
            raise SourceError(f"Cockpit Tools 第 {index_number} 个 Codex 账号索引与详情不一致")
        tokens = account.get("tokens")
        if not isinstance(tokens, dict):
            raise SourceError(f"Cockpit Tools 第 {index_number} 个 Codex 账号缺少 tokens")

        record = {
            "type": "codex",
            "email": required_string(
                account.get("email"),
                f"Cockpit Tools 第 {index_number} 个 Codex 账号 email",
            ),
            "access_token": required_string(
                tokens.get("access_token"),
                f"Cockpit Tools 第 {index_number} 个 Codex 账号 access_token",
            ),
        }
        for target_name, value in (
            ("id_token", tokens.get("id_token")),
            ("refresh_token", tokens.get("refresh_token")),
            ("account_id", account.get("account_id")),
        ):
            normalized = optional_string(value)
            if normalized:
                record[target_name] = normalized
        records.append(record)

    return records


def main() -> int:
    if sys.argv[1:] != ["--for-importer"] or sys.stdout.isatty():
        print(
            json.dumps(
                {"error": "请通过 import-codex-accounts.ps1 调用此组件"},
                ensure_ascii=True,
            )
        )
        return 1

    try:
        payload = {"records": load_records()}
        exit_code = 0
    except SourceError as error:
        payload = {"error": str(error)}
        exit_code = 1
    except Exception:
        payload = {"error": "读取 Cockpit Tools Codex 账号时发生未预期错误"}
        exit_code = 1

    print(json.dumps(payload, ensure_ascii=True, separators=(",", ":")))
    return exit_code


if __name__ == "__main__":
    sys.exit(main())
