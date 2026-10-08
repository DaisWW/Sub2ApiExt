"""按原始输入所在目录解析账户设置，不复制账户凭据。"""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any, Iterable, Mapping, Optional, Sequence

from inputs import InputFiles
from sub2api.main import ACCOUNT_CONFIG_FIELDS, AccountConfig, Sub2ApiError, comparable, json_file, record_key


class InputConfig:
    def __init__(self, config: Mapping[str, Any], inputs: InputFiles, input_dir: Path):
        self.inputs = inputs
        self.config_files: list[Path] = []
        root = input_dir.resolve()
        self.default = self._load(root, AccountConfig(config))
        directories = {path.parent.resolve() for path in inputs.accounts_files + inputs.codes_files}
        self.policies = {
            directory: self.default if directory == root else self._load(directory, self.default)
            for directory in sorted(directories)
        }

    def _load(self, directory: Path, parent: AccountConfig) -> AccountConfig:
        path = directory / "config.json"
        if not path.is_file():
            return parent
        value = json_file(path, "目录账户配置")
        if not isinstance(value, dict) or value.keys() - (ACCOUNT_CONFIG_FIELDS | {"defaults", "accounts", "redeem_codes"}):
            raise Sub2ApiError(f"目录配置只能包含账户设置、accounts 和 redeem_codes：{path}")
        self.config_files.append(path)
        try:
            return AccountConfig(value, parent=parent)
        except Sub2ApiError as exc:
            raise Sub2ApiError(f"目录配置无效：{path}；{exc}") from exc

    def compile(
        self, records: Sequence[dict[str, Any]], *, active_codes: Iterable[str] = (),
        code_accounts: Optional[Mapping[str, Optional[str]]] = None,
    ) -> tuple[dict[str, dict[str, Any]], dict[str, str]]:
        if code_accounts is not None and not isinstance(code_accounts, dict):
            raise Sub2ApiError("兑换码与账户的对应关系必须是 JSON 对象")
        configs, fingerprints = self.default.compile(records)
        active = set(active_codes)
        emails = {str(record.get("email", "")).strip().casefold(): record_key(record) for record in records}
        selected: dict[str, Path] = {}
        for directory, policy in self.policies.items():
            codes = [code for code in self.inputs.codes if code in active and directory in self.inputs.code_directories[code]]
            values, hashes = policy.compile(records, active_codes=codes, code_accounts=code_accounts)
            keys = {key for key, directories in self.inputs.record_directories.items() if directory in directories}
            for code in codes:
                digest = hashlib.sha256(code.encode("utf-8")).hexdigest()
                if code_accounts is None or digest not in code_accounts:
                    if policy is not self.default:
                        raise Sub2ApiError(f"兑换结果缺少目录内卡密的账户对应关系；已停止导入：{directory}")
                    continue
                email = code_accounts[digest]
                if email is None:
                    continue
                key = emails.get(email.strip().casefold()) if isinstance(email, str) else None
                if key is None:
                    if policy is not self.default:
                        raise Sub2ApiError(f"目录内卡密的账户无法匹配下载内容；已停止导入：{directory}")
                    continue
                keys.add(key)
            for key in sorted(keys):
                if key in selected and fingerprints[key] != hashes[key]:
                    fields = [name for name in configs[key] if name != "extra" and comparable(configs[key][name]) != comparable(values[key][name])]
                    fields.extend(
                        f"extra.{name}" for name in sorted(configs[key]["extra"].keys() | values[key]["extra"].keys())
                        if comparable(configs[key]["extra"].get(name)) != comparable(values[key]["extra"].get(name))
                    )
                    raise Sub2ApiError(
                        f"账户 {key} 的目录配置冲突：{selected[key]}、{directory}；字段：{', '.join(fields)}；已停止导入"
                    )
                configs[key], fingerprints[key] = values[key], hashes[key]
                selected[key] = directory
        return configs, fingerprints
