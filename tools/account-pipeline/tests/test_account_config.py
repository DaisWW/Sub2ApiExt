"""默认和邮箱配置以及增量设置同步的离线回归测试。"""

from __future__ import annotations

import argparse
import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import incremental
from inputs import InputFiles
import run as pipeline
from normalize import main as normalize
from sub2api import main as sub2api
from test_incremental_ownership import FakeClient, record, remote


class GroupClient(FakeClient):
    def request(self, method, path, *, token=None, body=None):
        if (method, path) == ("GET", "/admin/groups/all?include_inactive=true"):
            return [
                {"id": index, "name": name, "platform": "openai", "status": "active"}
                for index, name in enumerate(("西郊-gpt", "西郊-gpt-cursor", "附加分组"), 1)
            ]
        if (method, path) == ("GET", "/admin/proxies/all"):
            return [{"id": 7, "name": "Verge", "status": "active"}]
        return super().request(method, path, token=token, body=body)


class AccountConfigTests(unittest.TestCase):
    @staticmethod
    def input(root, accounts, *, name="accounts.txt", codes=None):
        path = root / "input" / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(accounts), encoding="utf-8")
        if codes is not None:
            (path.parent / "redeem-codes.txt").write_text(codes, encoding="utf-8")
        return path

    @staticmethod
    def prepare(root, config):
        inputs = InputFiles.discover(root / "input")
        inputs.load(allow_empty=False, conflict_file=root / "cache" / "results" / "input-conflicts.txt")
        metadata = normalize.normalize(None, root / "run", record_sources=inputs.sources)
        records = pipeline.normalized_records(Path(metadata["cockpit_input"]))
        configs, fingerprints = sub2api.AccountConfig(config).compile(records)
        incremental.write_json(Path(metadata["sub2api_input"]), incremental.sub2api_payload(records, configs))
        return metadata, configs, fingerprints

    @staticmethod
    def args(root, config_file, *, dry_run=False, incremental_mode=False, refresh_tokens=False):
        return argparse.Namespace(
            input_file=None, codes_file=None, accounts_file=None,
            runtime_dir=root / "cache", sub2api_config=config_file,
            wait_seconds=60, skip_sub2api=False, skip_cockpit=False,
            incremental=incremental_mode, refresh_tokens=refresh_tokens, dry_run=dry_run,
        )

    def test_email_overrides_defaults_and_extra_by_key(self):
        config = sub2api.AccountConfig({
            "defaults": {"group_names": ["西郊-gpt"], "concurrency": 3,
                         "extra": {"openai_passthrough": True, "codex_cli_only": True}},
            "accounts": {" SAME@Example.com ": {
                "group_names": ["西郊-gpt-cursor", "西郊-gpt", "西郊-gpt"],
                "concurrency": 1, "extra": {"codex_cli_only": False},
            }},
        })
        settings = config.resolve("same@example.com")
        self.assertEqual(settings["group_names"], ["西郊-gpt", "西郊-gpt-cursor"])
        self.assertEqual(settings["concurrency"], 1)
        self.assertFalse(settings["extra"]["codex_cli_only"])
        self.assertTrue(settings["extra"]["openai_passthrough"])
        self.assertTrue(config.resolve("other@example.com")["extra"]["codex_cli_only"])
        self.assertTrue(config.defaults["extra"]["codex_cli_only"])

    def test_group_array_replaces_defaults_and_can_be_cleared(self):
        config = sub2api.AccountConfig({
            "defaults": {"group_names": ["西郊-gpt"]},
            "accounts": {"one@example.com": {"group_names": ["西郊-gpt-cursor"]},
                         "none@example.com": {"group_names": []}},
        })
        self.assertEqual(config.resolve("one@example.com")["group_names"], ["西郊-gpt-cursor"])
        self.assertEqual(config.resolve("none@example.com")["group_names"], [])
        self.assertEqual(config.resolve("other@example.com")["group_names"], ["西郊-gpt"])

    def test_flat_configuration_remains_a_default(self):
        config = sub2api.AccountConfig({"concurrency": 2, "extra": {"codex_cli_only": True}})
        self.assertEqual(config.resolve("one@example.com")["concurrency"], 2)
        self.assertTrue(config.resolve("one@example.com")["extra"]["codex_cli_only"])

    def test_invalid_overrides_are_rejected_before_any_target_access(self):
        for config in (
            {"defaults": []}, {"accounts": []},
            {"accounts": {"bad": {}}},
            {"accounts": {"one@example.com": {"sub2api_url": "http://elsewhere"}}},
            {"accounts": {"one@example.com": {"concurrency": 0}}},
            {"accounts": {"one@example.com": {"extra": []}}},
            {"accounts": {"one@example.com": {}, " ONE@EXAMPLE.COM ": {}}},
        ):
            with self.subTest(config=config):
                with self.assertRaises(sub2api.Sub2ApiError):
                    sub2api.AccountConfig(config)

    def test_duplicate_json_keys_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            path.write_text('{"accounts":{"same@example.com":{},"same@example.com":{}}}', encoding="utf-8")
            with self.assertRaisesRegex(sub2api.Sub2ApiError, "重复 JSON 键"):
                sub2api.json_file(path, "配置")

    def test_multiple_input_files_deduplicate_without_directory_groups(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            same = record("same@example.com")
            self.input(root, [same, record("root@example.com")])
            self.input(root, [same], name="西郊-gpt-cursor/accounts-other.jsonl")
            self.input(root, [], name="accounts.example.txt")
            self.input(root, [], name="西郊-gpt-cursor/nested/accounts.txt")
            (root / "input" / "accounts.example.txt").write_text("invalid", encoding="utf-8")
            metadata, configs, _ = self.prepare(root, {"defaults": {"group_names": ["西郊-gpt"]}})
            self.assertEqual(metadata["accounts"], 2)
            self.assertEqual(configs["email:same@example.com"]["group_names"], ["西郊-gpt"])

    def test_credential_conflicts_remain_errors_without_token_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("same@example.com")])
            self.input(root, [{**record("same@example.com"), "access_token": "secret-conflicting-token"}], name="accounts-other.txt")
            with self.assertRaises(normalize.NormalizeError):
                self.prepare(root, {})
            report = (root / "cache" / "results" / "input-conflicts.txt").read_text(encoding="utf-8-sig")
            self.assertIn("access_token", report)
            self.assertNotIn("secret-conflicting-token", report)

    def test_configuration_delta_preserves_credentials_and_remote_extra(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            configured = {**record("settings@example.com"), "access_token": "expired-token"}
            refreshed, added = record("refreshed@example.com"), record("added@example.com")
            records = [configured, refreshed, added]
            self.input(root, records)
            _, configs, fingerprints = self.prepare(root, {
                "defaults": {"group_names": ["西郊-gpt"], "extra": {"codex_cli_only": False}},
            })
            previous = incremental.build_state(
                [configured, refreshed], {}, {"email:settings@example.com": 7, "email:refreshed@example.com": 8},
                config_fingerprints={**fingerprints, "email:settings@example.com": "old"},
            )
            delta = incremental.compare(records, previous, {}, refresh_keys=["email:refreshed@example.com"],
                                        config_fingerprints=fingerprints)
            paths = incremental.write_delta(root / "delta", delta, account_configs=configs)
            cockpit = pipeline.normalized_records(Path(paths["cockpit_input"]))
            self.assertEqual([item["email"] for item in cockpit], ["added@example.com", "refreshed@example.com"])
            existing = remote(7, "settings@example.com", managed=True)
            existing["credentials"] = {"access_token": "server-token", "refresh_token": "server-refresh"}
            existing["extra"]["unrelated_setting"] = "preserve"
            client = GroupClient([existing, remote(8, "refreshed@example.com", managed=True)])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", {}, client, "token")), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ):
                self.assertEqual(sub2api.run_import(Path(paths["sub2api_input"]), None), 0)
            settings_updates = [body for body in client.updates if body["name"] == "settings@example.com"]
            self.assertEqual(len(settings_updates), 1)
            self.assertNotIn("credentials", settings_updates[0])
            self.assertNotIn("content", settings_updates[0])
            self.assertEqual(client.accounts[7]["credentials"]["access_token"], "server-token")
            self.assertFalse(client.accounts[7]["extra"]["codex_cli_only"])
            self.assertEqual(client.accounts[7]["extra"]["unrelated_setting"], "preserve")
            self.assertEqual(client.accounts[7]["group_ids"], [1])
            self.assertEqual([json.loads(body["content"])["email"] for body in client.imports],
                             ["added@example.com", "refreshed@example.com"])

    def test_settings_only_does_not_recreate_missing_account(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            account = record("missing@example.com")
            self.input(root, [account])
            _, configs, fingerprints = self.prepare(root, {})
            previous = incremental.build_state([account], {}, {"email:missing@example.com": 7},
                                               config_fingerprints={"email:missing@example.com": "old"})
            delta = incremental.compare([account], previous, {}, config_fingerprints=fingerprints)
            paths = incremental.write_delta(root / "delta", delta, account_configs=configs)
            client = GroupClient([])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", {}, client, "token")), patch.object(
                sub2api, "get_accounts", return_value=[]
            ):
                with self.assertRaisesRegex(sub2api.Sub2ApiError, "仅更新设置"):
                    sub2api.run_import(Path(paths["sub2api_input"]), None)
            self.assertEqual(client.imports, [])
            self.assertEqual(client.updates, [])

    def test_settings_only_clears_expiry_load_factor_and_proxy(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            account = record("same@example.com")
            self.input(root, [account])
            _, configs, _ = self.prepare(root, {})
            path = root / "settings.json"
            incremental.write_json(path, incremental.sub2api_payload(
                [account], configs, settings_only_keys={"email:same@example.com"},
            ))
            existing = remote(7, "same@example.com", managed=True)
            existing.update(load_factor=8, expires_at=2000000000, proxy_id=7)
            client = GroupClient([existing])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", {}, client, "token")), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ):
                self.assertEqual(sub2api.run_import(path, None), 0)
            self.assertIsNone(client.accounts[7]["load_factor"])
            self.assertIsNone(client.accounts[7]["expires_at"])
            self.assertEqual(client.accounts[7]["proxy_id"], 0)
            self.assertEqual(client.imports, [])

    def test_incremental_payload_preserves_name_from_full_input(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = [record("first@example.com"), record("second@example.com")]
            self.input(root, accounts)
            _, configs, fingerprints = self.prepare(root, {
                "defaults": {"name_prefix": "account-", "name_start": 1, "name_width": 3},
            })
            previous = incremental.build_state(accounts, {}, {"email:first@example.com": 7, "email:second@example.com": 8},
                                               config_fingerprints={**fingerprints, "email:second@example.com": "old"})
            delta = incremental.compare(accounts, previous, {}, config_fingerprints=fingerprints)
            paths = incremental.write_delta(root / "delta", delta, account_configs=configs)
            client = GroupClient([remote(8, "second@example.com", managed=True)])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", {}, client, "token")), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ):
                self.assertEqual(sub2api.run_import(Path(paths["sub2api_input"]), None), 0)
            self.assertEqual(client.updates[0]["name"], "account-002")

    def test_standalone_import_applies_email_configuration(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps([record("same@example.com"), record("other@example.com")]), encoding="utf-8")
            config = {"defaults": {"group_names": ["西郊-gpt"], "proxy_name": "Verge"},
                      "accounts": {"same@example.com": {"group_names": ["西郊-gpt", "西郊-gpt-cursor"], "concurrency": 1}}}
            client = GroupClient([])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", config, client, "token")), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ):
                self.assertEqual(sub2api.run_import(path, None), 0)
            by_email = {json.loads(body["content"])["email"]: body for body in client.imports}
            self.assertEqual(by_email["same@example.com"]["group_ids"], [1, 2])
            self.assertEqual(by_email["same@example.com"]["concurrency"], 1)
            self.assertEqual(by_email["other@example.com"]["group_ids"], [1])
            self.assertTrue(all("config" not in json.loads(body["content"]) for body in client.imports))

    def test_full_import_applies_name_notes_and_clears_settings(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps({**record("same@example.com"), "refresh_token": "test-refresh"}), encoding="utf-8")
            existing = remote(7, "same@example.com", managed=True)
            existing.update(name="old", notes="old", group_ids=[1, 2], proxy_id=7, load_factor=8, expires_at=2000000000)
            client = GroupClient([existing])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", {}, client, "token")), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ):
                self.assertEqual(sub2api.run_import(path, None), 0)
            account = client.accounts[7]
            self.assertEqual(account["name"], "same@example.com")
            self.assertEqual(account["notes"], "")
            self.assertEqual(account["group_ids"], [])
            self.assertEqual(account["proxy_id"], 0)
            self.assertIsNone(account["load_factor"])
            self.assertIsNone(account["expires_at"])
            self.assertEqual(len(client.imports), 1)

    def test_full_access_token_only_import_preserves_upstream_expiry_policy(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps(record("same@example.com")), encoding="utf-8")
            existing = remote(7, "same@example.com", managed=True)
            existing.update(expires_at=2000000000, auto_pause_on_expired=True)
            client = GroupClient([existing])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", {}, client, "token")), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ):
                self.assertEqual(sub2api.run_import(path, None), 0)
            self.assertNotIn("expires_at", client.updates[0])
            self.assertNotIn("auto_pause_on_expired", client.updates[0])
            self.assertEqual(client.accounts[7]["expires_at"], 2000000000)
            self.assertTrue(client.accounts[7]["auto_pause_on_expired"])

    def test_all_groups_are_validated_before_first_import(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("first@example.com"), record("last@example.com")])
            metadata, _, _ = self.prepare(root, {
                "defaults": {"group_names": ["西郊-gpt"]},
                "accounts": {"last@example.com": {"group_names": ["不存在的分组"]}},
            })
            client = GroupClient([])
            with patch.object(sub2api, "admin_session", return_value=(root / "main.json", {}, client, "token")):
                with self.assertRaises(sub2api.Sub2ApiError):
                    sub2api.run_import(Path(metadata["sub2api_input"]), None)
            self.assertEqual(client.imports, [])

    def test_incremental_updates_email_settings_group_removal_and_defaults(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("same@example.com"), record("other@example.com")])
            config = {"defaults": {"group_names": ["西郊-gpt"]},
                      "accounts": {"same@example.com": {"group_names": ["西郊-gpt", "西郊-gpt-cursor"]}}}
            config_file = root / "main.json"
            config_file.write_text(json.dumps(config), encoding="utf-8")
            client = GroupClient([remote(7, "same@example.com", managed=True), remote(8, "other@example.com", managed=True)])
            ids = {"email:same@example.com": 7, "email:other@example.com": 8}
            with patch.object(pipeline, "INPUT_DIR", root / "input"), patch.object(
                sub2api, "admin_session", return_value=(config_file, config, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                sub2api, "resolve_account_ids", return_value=ids
            ), patch.object(pipeline.cockpit_module, "execute", side_effect=AssertionError("settings sent to Cockpit")):
                args = self.args(root, config_file, incremental_mode=True)
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 2)
                state = incremental.read_state(root / "cache" / "state" / "incremental.json")
                self.assertEqual(len(state["accounts"]["email:same@example.com"]["config_fingerprint"]), 64)
                self.assertNotIn("test-token", json.dumps(state))
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 2)
                config["accounts"]["same@example.com"]["priority"] = 80
                config_file.write_text(json.dumps(config), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 3)
                self.assertEqual(client.updates[-1]["priority"], 80)
                del config["accounts"]["same@example.com"]
                config_file.write_text(json.dumps(config), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 4)
                self.assertEqual(client.updates[-1]["group_ids"], [1])
                self.assertEqual(client.updates[-1]["priority"], 50)
                config["defaults"]["priority"] = 70
                config_file.write_text(json.dumps(config), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(len(client.updates), 6)
                self.assertTrue(all(body["priority"] == 70 for body in client.updates[-2:]))
                self.assertEqual(client.imports, [])
                self.assertFalse((root / "cache" / "state" / "token-snapshot.json").exists())

    def test_skipped_configuration_is_not_recorded_as_synced(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("same@example.com"), record("unchanged@example.com")])
            config = {"defaults": {"group_names": ["西郊-gpt"]}, "accounts": {}}
            config_file = root / "main.json"
            config_file.write_text(json.dumps(config), encoding="utf-8")
            client = GroupClient([remote(7, "same@example.com", managed=True), remote(8, "unchanged@example.com", managed=True)])
            ids = {"email:same@example.com": 7, "email:unchanged@example.com": 8}
            with patch.object(pipeline, "INPUT_DIR", root / "input"), patch.object(
                sub2api, "admin_session", return_value=(config_file, config, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                sub2api, "resolve_account_ids", return_value=ids
            ), patch.object(pipeline.cockpit_module, "execute", side_effect=AssertionError("settings sent to Cockpit")):
                args = self.args(root, config_file, incremental_mode=True)
                self.assertEqual(pipeline.run(args), 0)
                client.accounts[7]["extra"].pop(sub2api.OWNERSHIP_EXTRA_KEY)
                config["accounts"]["same@example.com"] = {"priority": 80}
                config_file.write_text(json.dumps(config), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
            state = incremental.read_state(root / "cache" / "state" / "incremental.json")
            self.assertNotIn("email:same@example.com", state["accounts"])
            self.assertIn("email:unchanged@example.com", state["accounts"])
            self.assertEqual(len(client.updates), 2)
            self.assertEqual(client.imports, [])

    def test_refresh_tokens_keeps_account_settings_untouched(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.input(root, [record("same@example.com")])
            config_file = root / "main.json"
            config_file.write_text(json.dumps({"accounts": {"same@example.com": {"priority": 80}}}), encoding="utf-8")
            result = sub2api.CredentialRefreshResult(updated={"email:same@example.com": 7})
            with patch.object(pipeline, "INPUT_DIR", root / "input"), patch.object(
                sub2api, "execute", side_effect=AssertionError("settings imported")
            ), patch.object(sub2api, "refresh_credentials", return_value=result) as refresh, patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(self.args(root, config_file, refresh_tokens=True)), 0)
                self.assertEqual(len(refresh.call_args.args[1]), 1)
                self.assertNotIn("config", refresh.call_args.args[1][0])


if __name__ == "__main__":
    unittest.main()
