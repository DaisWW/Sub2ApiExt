from __future__ import annotations

import json
import sys
import tempfile
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import incremental
import run as pipeline
import token_state
from sub2api import main as sub2api
import test_incremental_ownership as ownership
from test_incremental_ownership import record, remote
from test_sub2api_failure_isolation import FailingClient


class RecoveryClient(FailingClient):
    missing_status = 404
    failed_update_id = None

    def request(self, method, path, *, token=None, body=None):
        if method == "GET" and path.startswith("/admin/accounts/"):
            account_id = int(path.rsplit("/", 1)[1])
            if account_id not in self.accounts:
                cause = urllib.error.HTTPError(path, self.missing_status, "error", {}, None)
                raise sub2api.Sub2ApiError(f"账户查询失败（HTTP {self.missing_status}）") from cause
        if method == "PUT" and path == f"/admin/accounts/{self.failed_update_id}":
            raise sub2api.Sub2ApiError("创建后的设置更新失败")
        return super().request(method, path, token=token, body=body)


class IncrementalRemoteRecoveryTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.records = [record("missing@example.com"), record("existing@example.com")]
        self.accounts_file = self.root / "accounts.json"
        self.accounts_file.write_text(json.dumps(self.records), encoding="utf-8")
        self.config_file = self.root / "main.json"
        self.config = {}
        self.config_file.write_text("{}", encoding="utf-8")
        self.args = ownership.IncrementalOwnershipTests.pipeline_args(self.root, accounts=self.accounts_file)
        self.args.sub2api_config = self.config_file
        self.key = "email:missing@example.com"
        ids = {self.key: 7, "email:existing@example.com": 8}
        _, fingerprints = sub2api.AccountConfig({}).compile(self.records)
        self.fingerprint = fingerprints[self.key]
        self.state_file = self.root / "cache/state/incremental.json"
        incremental.write_state(self.state_file, incremental.build_state(
            self.records, incremental.input_scope(None, self.accounts_file), ids,
            config_fingerprints=fingerprints,
        ))
        self.token_file = self.root / "cache/state/token-snapshot.json"
        token_state.update_snapshot(self.token_file, self.records, ids)
        self.tokens_before = self.token_file.read_bytes()
        self.client = RecoveryClient([remote(8, "existing@example.com", managed=True)], failed_email=None)
        self.client.accounts[8]["credentials"] = {"access_token": "remote-newer-token"}
        patchers = [
            patch.object(pipeline, "load_pipeline_config", return_value={}),
            patch.object(sub2api, "admin_session", return_value=(self.config_file, self.config, self.client, "token")),
            patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(self.client.accounts.values())),
            patch.object(pipeline.cockpit_module, "execute", return_value=0),
        ]
        for patcher in patchers:
            self.addCleanup(patcher.stop)
        _, _, self.account_list, self.cockpit = [patcher.start() for patcher in patchers]

    def change_settings(self):
        self.config["defaults"] = {"priority": 80}
        self.config_file.write_text(json.dumps(self.config), encoding="utf-8")

    def assert_recovered(self):
        self.assertEqual(self.client.attempted, ["missing@example.com"])
        self.assertEqual(json.loads(self.cockpit.call_args.args[0].read_text(encoding="utf-8")), [self.records[0]])
        state = incremental.read_state(self.state_file)["accounts"]
        self.assertEqual(state[self.key]["sub2api_id"], 9)
        self.assertFalse(state[self.key].get("retry", False))
        self.assertEqual(state["email:existing@example.com"]["sub2api_id"], 8)
        self.assertEqual(token_state.read_snapshot(self.token_file)["accounts"][self.key]["sub2api_id"], 9)
        self.assertEqual(self.client.accounts[8]["credentials"]["access_token"], "remote-newer-token")

    def assert_recovery(self):
        self.assertEqual(pipeline.run(self.args), 0)
        self.assert_recovered()
        self.client.attempted.clear()
        updates = len(self.client.updates)
        self.cockpit.reset_mock()
        self.assertEqual(pipeline.run(self.args), 0)
        self.assertEqual(self.client.attempted, [])
        self.assertEqual(len(self.client.updates), updates)
        self.cockpit.assert_not_called()

    def assert_retry(self, *, cockpit_calls):
        self.assertEqual(pipeline.run(self.args), 1)
        self.assertEqual(self.cockpit.call_count, cockpit_calls)
        saved = incremental.read_state(self.state_file)["accounts"][self.key]
        self.assertEqual(saved["sub2api_id"], 7)
        self.assertEqual(saved["config_fingerprint"], self.fingerprint)
        self.assertTrue(saved.get("retry", False))
        self.assertEqual(self.token_file.read_bytes(), self.tokens_before)
        self.client.failed_update_id = None
        self.cockpit.side_effect = None
        self.cockpit.return_value = 0
        self.client.attempted.clear()
        self.assertEqual(pipeline.run(self.args), 0)
        self.assert_recovered()

    def test_missing_unchanged_account_is_recreated_and_then_skipped(self):
        self.assert_recovery()

    def test_missing_configured_account_is_recreated_without_reimporting_existing_tokens(self):
        self.change_settings()
        self.assert_recovery()
        self.assertTrue(all(account["priority"] == 80 for account in self.client.accounts.values()))

    def test_remote_list_failure_preserves_snapshots_and_stops_import(self):
        state_before = self.state_file.read_bytes()
        self.account_list.side_effect = sub2api.Sub2ApiError("账户列表查询失败")
        self.assertEqual(pipeline.run(self.args), 1)
        self.cockpit.assert_not_called()
        self.assertEqual(self.client.imports, [])
        self.assertEqual(self.client.updates, [])
        self.assertEqual(self.state_file.read_bytes(), state_before)
        self.assertEqual(self.token_file.read_bytes(), self.tokens_before)

    def test_old_id_query_error_does_not_recreate_account(self):
        for status in (401, 500):
            with self.subTest(status=status):
                self.client.missing_status = status
                self.assertEqual(pipeline.run(self.args), 1)
                self.assertEqual(self.client.imports, [])
                self.assertEqual(self.client.updates, [])
                self.cockpit.assert_not_called()
                saved = incremental.read_state(self.state_file)["accounts"][self.key]
                self.assertEqual(saved["sub2api_id"], 7)
                self.assertEqual(saved["config_fingerprint"], self.fingerprint)
                self.assertFalse(saved.get("retry", False))
                self.assertEqual(self.token_file.read_bytes(), self.tokens_before)

    def test_existing_old_id_with_changed_identity_is_not_recreated(self):
        self.client.accounts[7] = remote(7, "different@example.com", managed=True)
        self.assertEqual(pipeline.run(self.args), 1)
        self.cockpit.assert_not_called()
        self.assertEqual(self.client.imports, [])
        self.assertEqual(self.client.updates, [])
        self.assertEqual(self.client.accounts[7]["email"], "different@example.com")

    def test_detail_failure_is_isolated_while_missing_account_is_recovered(self):
        self.client.failed_detail_id = 8
        self.assertEqual(pipeline.run(self.args), 1)
        self.assert_recovered()

    def test_matching_manual_account_is_not_recreated_or_updated(self):
        manual = remote(7, "missing@example.com", managed=False)
        self.client.accounts[7] = manual
        self.change_settings()
        self.assertEqual(pipeline.run(self.args), 0)
        self.cockpit.assert_not_called()
        self.assertEqual(manual, remote(7, "missing@example.com", managed=False))
        self.assertEqual(self.client.imports, [])
        self.assertNotIn(self.key, incremental.read_state(self.state_file)["accounts"])

    def test_failed_recovery_keeps_old_snapshot_and_retries_credentials(self):
        self.client.failed_update_id = 9
        self.assert_retry(cockpit_calls=0)

    def test_matching_replacement_manual_account_is_not_adopted_into_snapshot(self):
        manual = remote(9, "missing@example.com", managed=False)
        self.client.accounts[9] = manual
        self.assertEqual(pipeline.run(self.args), 0)
        self.cockpit.assert_not_called()
        self.assertEqual(manual, remote(9, "missing@example.com", managed=False))
        self.assertEqual(self.client.imports, [])
        self.assertEqual(self.client.updates, [])
        saved = incremental.read_state(self.state_file)["accounts"][self.key]
        self.assertEqual(saved["sub2api_id"], 7)

    def test_cockpit_failure_keeps_recovery_pending_until_delivery(self):
        self.cockpit.return_value = 1
        self.assert_retry(cockpit_calls=1)

    def test_cockpit_exception_keeps_recovery_pending_until_delivery(self):
        self.cockpit.side_effect = pipeline.cockpit_module.CockpitError("投递失败")
        self.assert_retry(cockpit_calls=1)


if __name__ == "__main__":
    unittest.main()
