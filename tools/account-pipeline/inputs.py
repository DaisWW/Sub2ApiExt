"""读取账户文本和卡密，保留各账户和卡密所在目录。"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Dict, List, Tuple

from normalize import main as normalize
from redeem import main as redeem


@dataclass
class InputFiles:
    codes_files: List[Path]
    accounts_files: List[Path]
    codes: List[str] = field(default_factory=list, repr=False)
    sources: List[Tuple[Dict[str, Any], str]] = field(default_factory=list, repr=False)
    record_directories: Dict[str, set[Path]] = field(default_factory=dict, repr=False)
    code_directories: Dict[str, set[Path]] = field(default_factory=dict, repr=False)

    @classmethod
    def discover(cls, input_dir: Path) -> InputFiles:
        """读取根目录和一层子目录中的输入，排除示例文件。"""
        directories = [input_dir]
        if input_dir.is_dir():
            directories.extend(sorted(
                (path for path in input_dir.iterdir() if path.is_dir() and not path.is_symlink()),
                key=lambda path: path.name.casefold(),
            ))
        codes_files, accounts_files = [], []
        for directory in directories:
            codes_files.extend(sorted(
                path.resolve() for path in directory.glob("redeem-codes*.txt")
                if path.is_file() and ".example." not in path.name.lower()
            ))
            accounts_files.extend(sorted(
                path.resolve() for path in directory.glob("accounts*")
                if path.is_file() and path.suffix.lower() in {".txt", ".json", ".jsonl"}
                and ".example." not in path.name.lower()
            ))
        return cls(codes_files, accounts_files)

    def load(self, *, allow_empty: bool, conflict_file: Path) -> List[Dict[str, Any]]:
        self.sources = []
        self.record_directories = {}
        self.code_directories = {}
        for path in self.accounts_files:
            raw = normalize.records_from_values(normalize.read_json(path, "账户文本", allow_empty=True))
            for index, record in enumerate(raw, 1):
                canonical = normalize.canonical_record(record, index)
                self.sources.append((canonical, f"accounts-file:{path}"))
                self.record_directories.setdefault(normalize.record_key(canonical), set()).add(path.parent.resolve())
        records, _, conflicts = normalize.deduplicate_with_sources(self.sources)
        normalize.write_conflict_report(conflict_file, conflicts)
        if conflicts:
            raise normalize.NormalizeError(
                f"发现 {len(conflicts)} 个同账号凭据冲突；已停止导入，详见：{conflict_file}"
            )
        for path in self.codes_files:
            for code in redeem.read_codes(path, allow_blank=True):
                self.code_directories.setdefault(code, set()).add(path.parent.resolve())
        self.codes = list(self.code_directories)
        if len(self.codes) > redeem.MAX_CODES or len("\n".join(self.codes).encode("utf-8")) > redeem.MAX_TEXT_BYTES:
            raise redeem.RedeemError("合并后的卡密数量或内容超过兑换站限制")
        if len(records) > normalize.MAX_ACCOUNTS:
            raise normalize.NormalizeError(f"账号数量超过 {normalize.MAX_ACCOUNTS} 个")
        if not self.codes and not records and not allow_empty:
            raise normalize.NormalizeError("输入中没有可用卡密或账户；请填写卡密或账户文本")
        return records
