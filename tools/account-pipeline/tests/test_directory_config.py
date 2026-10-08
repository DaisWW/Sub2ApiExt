"""目录配置、轻量覆盖和移动账户后的增量同步回归测试。"""

import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from input_config import InputConfig
from inputs import InputFiles
import run as pipeline
from redeem import main as redeem
from sub2api import main as sub2api
import test_account_config as config_tests
from test_account_config import GroupClient
from test_incremental_ownership import record, remote


class DirectoryConfigTests(unittest.TestCase):
    input = staticmethod(config_tests.AccountConfigTests.input)
    prepare = staticmethod(config_tests.AccountConfigTests.prepare)
    args = staticmethod(config_tests.AccountConfigTests.args)

    @staticmethod
    def configuration(root, folder, value):
        path = root / "input" / folder / "config.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(value), encoding="utf-8")
        return path

    def test_directory_settings_inherit_defaults_without_copying_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("ordinary@example.com")], name="普通/accounts.txt")
            self.input(root, [record("cursor@example.com")], name="cursor/accounts.json")
            self.configuration(root, "cursor", {
                "group_names": ["西郊-gpt", "西郊-gpt-cursor"], "extra": {"codex_cli_only": False},
            })
            _, configs, _ = self.prepare(root, {
                "defaults": {"group_names": ["西郊-gpt"], "concurrency": 3, "rate_multiplier": 0.1,
                             "extra": {"codex_cli_only": True, "openai_passthrough": True}},
            })
            ordinary, cursor = configs["email:ordinary@example.com"], configs["email:cursor@example.com"]
            self.assertEqual(ordinary["group_names"], ["西郊-gpt"])
            self.assertTrue(ordinary["extra"]["codex_cli_only"])
            self.assertEqual(cursor["group_names"], ["西郊-gpt", "西郊-gpt-cursor"])
            self.assertFalse(cursor["extra"]["codex_cli_only"])
            self.assertTrue(cursor["extra"]["openai_passthrough"])
            self.assertEqual(cursor["concurrency"], 3)
            self.assertEqual(cursor["rate_multiplier"], 0.1)
            self.assertNotIn("access_token", json.dumps(configs))

    def test_root_and_directory_email_overrides_merge_by_key_and_stay_local(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("one@example.com")], name="cursor/accounts.txt")
            self.input(root, [record("other@example.com")], name="ordinary/accounts.txt")
            self.configuration(root, "", {"priority": 70, "accounts": {"one@example.com": {"extra": {"flag": True}}}})
            self.configuration(root, "cursor", {
                "accounts": {" ONE@EXAMPLE.COM ": {"extra": {"codex_cli_only": False}},
                             "other@example.com": {"concurrency": 1}},
            })
            _, configs, _ = self.prepare(root, {"defaults": {"extra": {"codex_cli_only": True}}})
            self.assertTrue(configs["email:one@example.com"]["extra"]["flag"])
            self.assertFalse(configs["email:one@example.com"]["extra"]["codex_cli_only"])
            self.assertEqual(configs["email:other@example.com"]["concurrency"], 3)
            self.assertTrue(all(value["priority"] == 70 for value in configs.values()))

    def test_multiple_code_files_deduplicate_and_ignore_examples(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("one@example.com")], codes="# DISABLED\n")
            (root / "input" / "redeem-codes-batch.txt").write_text("FIRST\nSECOND\nFIRST\n", encoding="utf-8")
            (root / "input" / "redeem-codes.example.txt").write_text("EXAMPLE\n", encoding="utf-8")
            inputs = InputFiles.discover(root / "input")
            inputs.load(allow_empty=False, conflict_file=root / "conflicts.txt")
            self.assertEqual(inputs.codes, ["FIRST", "SECOND"])
            self.assertEqual(len(inputs.codes_files), 2)

    def test_conflicting_directory_settings_fail_without_credential_values(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for folder in ("ordinary", "cursor"):
                self.input(root, [record("same@example.com")], name=f"{folder}/accounts.txt")
            self.configuration(root, "cursor", {"extra": {"codex_cli_only": False}})
            with self.assertRaisesRegex(sub2api.Sub2ApiError, "extra.codex_cli_only") as error:
                self.prepare(root, {"defaults": {"extra": {"codex_cli_only": True}}})
            self.assertIn("ordinary", str(error.exception))
            self.assertIn("cursor", str(error.exception))
            self.assertNotIn("test-token", str(error.exception))

    def test_duplicate_accounts_with_equal_settings_are_allowed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for folder in ("ordinary", "cursor"):
                self.input(root, [record("same@example.com")], name=f"{folder}/accounts.txt")
            self.configuration(root, "cursor", {"concurrency": 3})
            metadata, configs, _ = self.prepare(root, {})
            self.assertEqual(metadata["accounts"], 1)
            self.assertEqual(len(configs), 1)

    def test_directory_settings_with_different_json_types_are_conflicting(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for folder in ("ordinary", "cursor"):
                self.input(root, [record("same@example.com")], name=f"{folder}/accounts.txt")
            self.configuration(root, "cursor", {"extra": {"flag": 0}})
            with self.assertRaisesRegex(sub2api.Sub2ApiError, "extra.flag"):
                self.prepare(root, {"defaults": {"extra": {"flag": False}}})

    def test_directory_codes_require_bindings_and_failed_codes_are_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [], name="cursor/accounts.txt", codes="PRIVATE-CARD\n")
            self.configuration(root, "cursor", {"extra": {"codex_cli_only": False}})
            inputs = InputFiles.discover(root / "input")
            inputs.load(allow_empty=False, conflict_file=root / "conflicts.txt")
            config = InputConfig({}, inputs, root / "input")
            for bindings in (None, {}, redeem.code_account_bindings([
                {"code": "PRIVATE-CARD", "account": "missing@example.com", "ok": True},
            ])):
                with self.subTest(bindings=bindings), self.assertRaises(sub2api.Sub2ApiError) as error:
                    config.compile([record("one@example.com")], active_codes=inputs.codes, code_accounts=bindings)
                self.assertNotIn("PRIVATE-CARD", str(error.exception))
            values, _ = config.compile([record("one@example.com")], active_codes=inputs.codes,
                                      code_accounts=redeem.code_account_bindings([{"code": "PRIVATE-CARD", "ok": False}]))
            self.assertEqual(values["email:one@example.com"]["group_names"], [])

    def test_directory_codes_inherit_settings_and_apply_code_then_email_override(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [], name="cursor/accounts.txt", codes="CARD\nFAILED\n")
            self.configuration(root, "cursor", {
                "group_names": ["西郊-gpt", "西郊-gpt-cursor"], "extra": {"codex_cli_only": False},
                "redeem_codes": {"CARD": {"priority": 70}},
                "accounts": {"received@example.com": {"priority": 80}},
            })
            config_file = root / "main.json"
            config = {"defaults": {"group_names": ["西郊-gpt"], "extra": {"codex_cli_only": True}}}
            config_file.write_text(json.dumps(config), encoding="utf-8")

            def fake_redeem(input_path, *, run_dir, result_file, manifest_file, **kwargs):
                self.assertEqual(input_path.read_text(encoding="utf-8").splitlines(), ["CARD", "FAILED"])
                data = run_dir / "data"
                data.mkdir(parents=True)
                (data / "accounts.json").write_text(json.dumps(record("received@example.com")), encoding="utf-8")
                redeem.write_manifest(manifest_file, task_id="test-task", result_file=result_file, data_dir=data,
                                      rows=[{"code": "CARD", "account": "received@example.com", "ok": True},
                                            {"code": "FAILED", "ok": False}])
                return 2

            client = GroupClient([])
            with patch.object(pipeline, "INPUT_DIR", root / "input"), patch.object(redeem, "execute", side_effect=fake_redeem), patch.object(
                sub2api, "admin_session", return_value=(config_file, config, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(self.args(root, config_file)), 0)
            self.assertEqual(client.imports[0]["group_ids"], [1, 2])
            self.assertEqual(client.imports[0]["priority"], 80)
            self.assertFalse(client.imports[0]["extra"]["codex_cli_only"])

    def test_moving_account_and_removing_directory_config_syncs_settings_only(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("one@example.com"), record("other@example.com")], name="ordinary/accounts.txt")
            config = {"defaults": {"group_names": ["西郊-gpt"], "extra": {"codex_cli_only": True}}}
            config_file = root / "main.json"
            config_file.write_text(json.dumps(config), encoding="utf-8")
            client = GroupClient([remote(7, "one@example.com", managed=True), remote(8, "other@example.com", managed=True)])
            ids = {"email:one@example.com": 7, "email:other@example.com": 8}
            with patch.object(pipeline, "INPUT_DIR", root / "input"), patch.object(
                sub2api, "admin_session", return_value=(config_file, config, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                sub2api, "resolve_account_ids", return_value=ids
            ), patch.object(pipeline.cockpit_module, "execute", side_effect=AssertionError("settings sent to Cockpit")):
                args = self.args(root, config_file, incremental_mode=True)
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 2)
                self.input(root, [record("other@example.com")], name="ordinary/accounts.txt")
                self.input(root, [record("one@example.com")], name="cursor/accounts.txt")
                folder_config = self.configuration(root, "cursor", {
                    "group_names": ["西郊-gpt", "西郊-gpt-cursor"], "extra": {"codex_cli_only": False},
                })
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 3)
                self.assertEqual(client.updates[-1]["group_ids"], [1, 2])
                self.assertFalse(client.updates[-1]["extra"]["codex_cli_only"])
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 3)
                folder_config.unlink()
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 4)
                self.assertEqual(client.updates[-1]["group_ids"], [1])
                self.assertTrue(client.updates[-1]["extra"]["codex_cli_only"])
            self.assertEqual(client.imports, [])
            self.assertTrue(all("credentials" not in value for value in client.updates))

    def test_invalid_directory_config_stops_all_modes_before_network_access(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("one@example.com")], name="cursor/accounts.txt", codes="CARD\n")
            config_file = root / "main.json"
            config_file.write_text("{}", encoding="utf-8")
            for value in ({"access_token": "PRIVATE"}, {"sub2api_url": "http://elsewhere"}, {"extra": []},
                          {"redeem_codes": {"PRIVATE-CARD": {"priority": 0}}}):
                self.configuration(root, "cursor", value)
                for mode in ({}, {"incremental_mode": True}, {"refresh_tokens": True}):
                    with self.subTest(value=value, mode=mode), patch.object(pipeline, "INPUT_DIR", root / "input"), patch.object(
                        sub2api, "admin_session", side_effect=AssertionError("logged in")
                    ), patch.object(redeem, "execute", side_effect=AssertionError("redeemed")), contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
                        with self.assertRaises(sub2api.Sub2ApiError) as error:
                            pipeline.run(self.args(root, config_file, dry_run=True, **mode))
                        self.assertNotIn("PRIVATE", str(error.exception))

    def test_dragged_account_file_uses_its_directory_config(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = self.input(root, [record("one@example.com")], name="cursor/accounts.txt")
            self.configuration(root, "cursor", {"concurrency": 1})
            inputs = InputFiles([], [accounts])
            records = inputs.load(allow_empty=False, conflict_file=root / "conflicts.txt")
            configs, _ = InputConfig({}, inputs, root / "input").compile(records)
            self.assertEqual(configs["email:one@example.com"]["concurrency"], 1)


if __name__ == "__main__":
    unittest.main()
