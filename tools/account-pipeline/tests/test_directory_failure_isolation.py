"""目录配置与逐账户失败隔离的兼容回归测试。"""

from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from input_config import InputConfig
from inputs import InputFiles
from redeem import main as redeem
import test_directory_config as directory_tests
from test_incremental_ownership import record


class DirectoryFailureIsolationTests(unittest.TestCase):
    input = staticmethod(directory_tests.DirectoryConfigTests.input)
    configuration = staticmethod(directory_tests.DirectoryConfigTests.configuration)

    @staticmethod
    def load(root):
        inputs = InputFiles.discover(root / "input")
        records = inputs.load(allow_empty=False, conflict_file=root / "conflicts.txt")
        return inputs, records

    def test_unused_invalid_directory_override_does_not_affect_other_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("ordinary@example.com")], name="ordinary/accounts.txt")
            self.input(root, [record("cursor@example.com")], name="cursor/accounts.txt")
            self.configuration(root, "cursor", {
                "accounts": {"ordinary@example.com": {"concurrency": 0}},
            })
            inputs, records = self.load(root)
            failures = {}
            configs, _ = InputConfig({}, inputs, root / "input").compile(records, failed_accounts=failures)
            self.assertEqual(failures, {})
            self.assertEqual(len(configs), 2)

    def test_invalid_global_email_override_is_inherited(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("bad@example.com"), record("good@example.com")], name="cursor/accounts.txt")
            self.configuration(root, "cursor", {"priority": 70})
            inputs, records = self.load(root)
            failures = {}
            configs, hashes = InputConfig({"accounts": {"bad@example.com": {"concurrency": 0}}}, inputs,
                                          root / "input").compile(records, failed_accounts=failures)
            self.assertEqual(set(failures), {"email:bad@example.com"})
            self.assertEqual(set(configs), {"email:good@example.com"})
            self.assertEqual(set(hashes), set(configs))
            self.assertEqual(configs["email:good@example.com"]["priority"], 70)

    def test_invalid_local_override_is_isolated(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("bad@example.com")], name="cursor/accounts.txt")
            self.input(root, [record("good@example.com")], name="ordinary/accounts.txt")
            self.configuration(root, "cursor", {"accounts": {"bad@example.com": {"concurrency": 0}}})
            inputs, records = self.load(root)
            failures = {}
            configs, hashes = InputConfig({}, inputs, root / "input").compile(records, failed_accounts=failures)
            self.assertEqual(set(failures), {"email:bad@example.com"})
            self.assertEqual(set(configs), {"email:good@example.com"})
            self.assertEqual(set(hashes), set(configs))

    def test_invalid_global_code_override_is_inherited(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("good@example.com")], name="ordinary/accounts.txt")
            self.input(root, [], name="cursor/accounts.txt", codes="CARD\n")
            self.configuration(root, "cursor", {"priority": 70})
            inputs, records = self.load(root)
            failures = {}
            records.append(record("bad@example.com"))
            configs, _ = InputConfig({"redeem_codes": {"CARD": {"concurrency": 0}}}, inputs,
                                     root / "input").compile(
                records, active_codes=inputs.codes,
                code_accounts=redeem.code_account_bindings([{"code": "CARD", "ok": True, "account": "bad@example.com"}]),
                failed_accounts=failures,
            )
            self.assertEqual(set(failures), {"email:bad@example.com"})
            self.assertEqual(set(configs), {"email:good@example.com"})

    def test_directory_conflict_isolates_only_the_conflicting_account(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("same@example.com"), record("good@example.com")], name="ordinary/accounts.txt")
            self.input(root, [record("same@example.com")], name="cursor/accounts.txt")
            self.configuration(root, "cursor", {"extra": {"codex_cli_only": False}})
            inputs, records = self.load(root)
            failures = {}
            configs, hashes = InputConfig({"extra": {"codex_cli_only": True}}, inputs,
                                          root / "input").compile(records, failed_accounts=failures)
            self.assertEqual(set(failures), {"email:same@example.com"})
            self.assertIn("extra.codex_cli_only", failures["email:same@example.com"])
            self.assertEqual(set(configs), {"email:good@example.com"})
            self.assertEqual(set(hashes), set(configs))

    def test_filtered_records_do_not_leave_stale_directory_keys(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("bad@example.com"), record("good@example.com")], name="cursor/accounts.txt")
            self.configuration(root, "cursor", {"priority": 70})
            inputs, records = self.load(root)
            failures = {"email:bad@example.com": "输入已隔离"}
            records = [value for value in records if value["email"] == "good@example.com"]
            configs, _ = InputConfig({}, inputs, root / "input").compile(records, failed_accounts=failures)
            self.assertEqual(set(configs), {"email:good@example.com"})
            self.assertEqual(configs["email:good@example.com"]["priority"], 70)

    def test_directory_compilation_preserves_global_name_indices(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("cursor@example.com")], name="cursor/accounts.txt")
            self.input(root, [record("ordinary@example.com")], name="ordinary/accounts.txt")
            self.configuration(root, "cursor", {"priority": 70})
            inputs, records = self.load(root)
            configs, _ = InputConfig({"name_prefix": "account-", "name_start": 100}, inputs,
                                     root / "input").compile(records, failed_accounts={})
            for index, value in enumerate(records):
                self.assertEqual(configs["email:" + value["email"]]["name"], f"account-{100 + index:03d}")

    def test_missing_download_for_known_directory_code_is_isolated(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("good@example.com")], name="ordinary/accounts.txt")
            self.input(root, [], name="cursor/accounts.txt", codes="CARD\n")
            self.configuration(root, "cursor", {"priority": 70})
            inputs, records = self.load(root)
            failures = {}
            configs, _ = InputConfig({}, inputs, root / "input").compile(
                records, active_codes=inputs.codes,
                code_accounts=redeem.code_account_bindings([{"code": "CARD", "ok": True, "account": "bad@example.com"}]),
                failed_accounts=failures,
            )
            self.assertEqual(set(failures), {"email:bad@example.com"})
            self.assertEqual(set(configs), {"email:good@example.com"})


if __name__ == "__main__":
    unittest.main()
