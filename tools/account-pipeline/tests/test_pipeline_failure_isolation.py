from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import contextlib
import io
import incremental
import token_state
import run as pipeline
from sub2api import main as sub2api
import test_incremental_ownership as ownership
import test_token_refresh as token_tests
from test_incremental_ownership import record, remote
from test_token_refresh import CredentialClient
from test_sub2api_failure_isolation import FailingClient


class PipelineFailureIsolationTests(unittest.TestCase):
    def test_incremental_partial_import_delivers_successes_and_retries_only_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            records = [record(email) for email in ("first@example.com", "bad@example.com", "last@example.com")]
            accounts.write_text(json.dumps(records), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=accounts)
            args.sub2api_config = config
            client = FailingClient()
            delivered = []

            def cockpit(path, **_kwargs):
                delivered.append([r["email"] for r in json.loads(path.read_text(encoding="utf-8"))])
                return 0

            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", side_effect=cockpit
            ):
                self.assertEqual(pipeline.run(args), 1)
                self.assertEqual(delivered, [["first@example.com", "last@example.com"]])
                state_path = root / "cache/state/incremental.json"
                self.assertEqual(set(incremental.read_state(state_path)["accounts"]), {"email:first@example.com", "email:last@example.com"})
                manifests = list((root / "cache/runs").glob("*/manifest.json"))
                self.assertEqual(json.loads(manifests[0].read_text(encoding="utf-8"))["status"], "partial_failure")
                client.attempted.clear()
                client.failed_email = None
                self.assertEqual(pipeline.run(args), 0)
            self.assertEqual(client.attempted, ["bad@example.com"])
            self.assertEqual(delivered[-1], ["bad@example.com"])
            self.assertEqual(len(incremental.read_state(state_path)["accounts"]), 3)

    def test_input_failure_preserves_previous_account_and_avoids_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            bad = record("bad@example.com")
            path.write_text(json.dumps([bad, {**bad, "access_token": "different"}, record("good@example.com")]), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            state_path = root / "cache/state/incremental.json"
            incremental.write_state(state_path, incremental.build_state([bad], incremental.input_scope(None, path), {"email:bad@example.com": 7}))
            client = FailingClient([remote(7, "bad@example.com", managed=True)])
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 1)
                state = incremental.read_state(state_path)
                self.assertEqual(state["accounts"]["email:bad@example.com"]["sub2api_id"], 7)
                self.assertFalse(state["accounts"]["email:bad@example.com"]["retry"])
                self.assertIn("email:good@example.com", state["accounts"])
                self.assertNotIn("bad@example.com", (root / "cache/results/sub2api-pending-deletions.txt").read_text(encoding="utf-8-sig"))
                client.failed_email = None
                client.attempted.clear()
                path.write_text(json.dumps([bad, record("good@example.com")]), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(client.attempted, [])

    def test_failed_settings_retry_does_not_import_old_credentials(self):
        class Client(FailingClient):
            blocked = True

            def request(self, method, path, *, token=None, body=None):
                if self.blocked and (method, path) == ("PUT", "/admin/accounts/7"):
                    raise sub2api.Sub2ApiError("账户设置更新失败")
                return super().request(method, path, token=token, body=body)

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            records = [record("bad@example.com"), record("good@example.com")]
            path = root / "accounts.json"
            path.write_text(json.dumps(records), encoding="utf-8")
            config = root / "config.json"
            config_value = {"accounts": {"bad@example.com": {"priority": 80}}}
            config.write_text(json.dumps(config_value), encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            _, fingerprints = sub2api.AccountConfig({}).compile(records)
            state_path = root / "cache/state/incremental.json"
            incremental.write_state(state_path, incremental.build_state([records[0]], incremental.input_scope(None, path), {"email:bad@example.com": 7}, config_fingerprints=fingerprints))
            client = Client([remote(7, "bad@example.com", managed=True)], failed_email=None)
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, config_value, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 1)
                self.assertEqual(incremental.read_state(state_path)["accounts"]["email:bad@example.com"]["config_fingerprint"], fingerprints["email:bad@example.com"])
                client.blocked = False
                self.assertEqual(pipeline.run(args), 0)
            self.assertEqual(client.attempted, ["good@example.com"])
            self.assertEqual(client.accounts[7]["priority"], 80)

    def test_invalid_input_file_does_not_block_other_files_or_create_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            input_dir = root / "input"
            input_dir.mkdir()
            (input_dir / "accounts-bad.json").write_text('{"access_token":"secret-truncated', encoding="utf-8")
            (input_dir / "accounts-good.json").write_text(json.dumps(record("good@example.com")), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root)
            args.sub2api_config = config
            state_path = root / "cache/state/incremental.json"
            incremental.write_state(state_path, incremental.build_state(
                [record("old@example.com")], {"input_dir": str(input_dir.resolve())}, {"email:old@example.com": 7},
            ))
            client = FailingClient([remote(7, "old@example.com", managed=True)])
            with patch.object(pipeline, "INPUT_DIR", input_dir), patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 1)
            state = incremental.read_state(state_path)["accounts"]
            self.assertEqual(client.attempted, ["good@example.com"])
            self.assertEqual(state["email:old@example.com"]["sub2api_id"], 7)
            self.assertFalse(state["email:old@example.com"]["retry"])
            self.assertIn("email:good@example.com", state)
            for name in ("sub2api", "cockpit"):
                self.assertNotIn("old@example.com", (root / f"cache/results/{name}-pending-deletions.txt").read_text(encoding="utf-8-sig"))
            manifest = next((root / "cache/runs").glob("*/manifest.json")).read_text(encoding="utf-8")
            self.assertNotIn("secret-truncated", manifest)

    def test_full_pipeline_delivers_successful_accounts_after_middle_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps([record(email) for email in ("first@example.com", "bad@example.com", "last@example.com")]), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path, incremental_mode=False)
            args.sub2api_config = config
            client = FailingClient()
            delivered = []

            def cockpit(input_path, **_kwargs):
                delivered.extend(r["email"] for r in pipeline.normalized_records(input_path))
                return 0

            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", side_effect=cockpit
            ):
                self.assertEqual(pipeline.run(args), 1)
            self.assertEqual(client.attempted, ["first@example.com", "bad@example.com", "last@example.com"])
            self.assertEqual(delivered, ["first@example.com", "last@example.com"])
            self.assertEqual(set(token_state.read_snapshot(root / "cache/state/token-snapshot.json")["accounts"]), {
                "email:first@example.com", "email:last@example.com",
            })

    def test_failed_input_with_only_account_id_preserves_previous_email_snapshot(self):
        for invalid in ({"account_id": "old-id"}, {"account_id": "old-id", "access_token": "token"}):
            with self.subTest(invalid=invalid), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path = root / "accounts.json"
                path.write_text(json.dumps([invalid, record("good@example.com")]), encoding="utf-8")
                config = root / "config.json"
                config.write_text("{}", encoding="utf-8")
                args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
                args.sub2api_config = config
                state_path = root / "cache/state/incremental.json"
                incremental.write_state(state_path, incremental.build_state(
                    [{**record("old@example.com"), "account_id": "old-id"}], incremental.input_scope(None, path),
                    {"email:old@example.com": 7},
                ))
                client = FailingClient([remote(7, "old@example.com", managed=True)])
                with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                    sub2api, "admin_session", return_value=(config, {}, client, "token")
                ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                    pipeline.cockpit_module, "execute", return_value=0
                ):
                    self.assertEqual(pipeline.run(args), 1)
                state = incremental.read_state(state_path)["accounts"]
                self.assertEqual(state["email:old@example.com"]["sub2api_id"], 7)
                self.assertFalse(state["email:old@example.com"]["retry"])
                self.assertEqual(client.attempted, ["good@example.com"])
                self.assertNotIn("old@example.com", (root / "cache/results/sub2api-pending-deletions.txt").read_text(encoding="utf-8-sig"))

    def test_token_refresh_partial_failure_advances_only_successful_snapshots(self):
        class Client(CredentialClient):
            blocked = True

            def request(self, method, path, *, token=None, body=None):
                if self.blocked and (method, path) == ("POST", "/admin/accounts/bulk-update") and body["account_ids"] == [7]:
                    raise sub2api.Sub2ApiError("凭据更新失败")
                return super().request(method, path, token=token, body=body)

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            records = [{**record(email), "access_token": "new-token"} for email in ("bad@example.com", "good@example.com")]
            path = root / "accounts.json"
            path.write_text(json.dumps(records), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = token_tests.TokenRefreshTests.pipeline_args(root, path)
            args.sub2api_config = config
            snapshot_path = root / "cache/state/token-snapshot.json"
            token_state.update_snapshot(snapshot_path, [record("bad@example.com")], {"email:bad@example.com": 7})
            client = Client([remote(7, "bad@example.com", managed=True), remote(8, "good@example.com", managed=True)])
            for account in client.accounts.values():
                account["credentials"] = {}
            delivered = []

            def cockpit(input_path, **_kwargs):
                delivered.append([r["email"] for r in pipeline.normalized_records(input_path)])
                return 0

            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", side_effect=cockpit
            ):
                self.assertEqual(pipeline.run(args), 1)
                snapshot = token_state.read_snapshot(snapshot_path)["accounts"]
                self.assertEqual(snapshot["email:bad@example.com"]["access_token"], "test-token")
                self.assertEqual(snapshot["email:good@example.com"]["access_token"], "new-token")
                manifest = json.loads(next((root / "cache/runs").glob("*/manifest.json")).read_text(encoding="utf-8"))
                self.assertEqual(manifest["status"], "partial_failure")
                self.assertEqual(set(manifest["account_failures"]), {"email:bad@example.com"})
                client.blocked = False
                client.updates.clear()
                self.assertEqual(pipeline.run(args), 0)
            self.assertEqual(delivered, [["good@example.com"], ["bad@example.com"]])
            self.assertEqual([body["account_ids"] for _, body in client.updates], [[7]])

    def test_baseline_detail_failure_keeps_manual_account_protection_and_imports_new_account(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps([record(email) for email in ("manual@example.com", "bad@example.com", "new@example.com")]), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            client = FailingClient([remote(7, "manual@example.com", managed=False), remote(8, "bad@example.com", managed=True)], failed_detail_id=8)
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 1)
            self.assertEqual(client.attempted, ["new@example.com"])
            state = incremental.read_state(root / "cache/state/incremental.json")
            self.assertEqual(set(state["accounts"]), {"email:new@example.com"})
            self.assertFalse(sub2api.is_tool_managed(client.accounts[7]))

    def test_invalid_local_configuration_retries_settings_without_old_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            records = [record("bad@example.com"), record("good@example.com")]
            path = root / "accounts.json"
            path.write_text(json.dumps(records), encoding="utf-8")
            config = root / "config.json"
            config_value = {"accounts": {"bad@example.com": {"priority": 0}}}
            config.write_text(json.dumps(config_value), encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            _, fingerprints = sub2api.AccountConfig({}).compile(records)
            state_path = root / "cache/state/incremental.json"
            incremental.write_state(state_path, incremental.build_state(
                [records[0]], incremental.input_scope(None, path), {"email:bad@example.com": 7}, config_fingerprints=fingerprints,
            ))
            client = FailingClient([remote(7, "bad@example.com", managed=True)], failed_email=None)
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, config_value, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 1)
                state = incremental.read_state(state_path)["accounts"]["email:bad@example.com"]
                self.assertFalse(state["retry"])
                self.assertEqual(state["config_fingerprint"], fingerprints["email:bad@example.com"])
                config_value["accounts"]["bad@example.com"]["priority"] = 80
                config.write_text(json.dumps(config_value), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
            self.assertEqual(client.attempted, ["good@example.com"])
            self.assertEqual(client.accounts[7]["priority"], 80)

    def test_shared_admin_login_failure_stops_without_delivering_to_cockpit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps(record("good@example.com")), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path, incremental_mode=False)
            args.sub2api_config = config
            with contextlib.redirect_stderr(io.StringIO()), patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", side_effect=sub2api.Sub2ApiError("管理员登录失败")
            ), patch.object(pipeline.cockpit_module, "execute") as cockpit:
                self.assertEqual(pipeline.run(args), 1)
            cockpit.assert_not_called()
            manifest = json.loads(next((root / "cache/runs").glob("*/manifest.json")).read_text(encoding="utf-8"))
            self.assertEqual(manifest["status"], "failed")
            self.assertEqual(manifest["failed_stage"], "sub2api")

    def test_token_only_mode_ignores_unrelated_invalid_account_settings(self):
        for config_value in ({"defaults": {"priority": 0}}, {"accounts": {"good@example.com": {"extra": []}}}):
            with self.subTest(config_value=config_value), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path = root / "accounts.json"
                path.write_text(json.dumps(record("good@example.com")), encoding="utf-8")
                config = root / "config.json"
                config.write_text(json.dumps(config_value), encoding="utf-8")
                args = token_tests.TokenRefreshTests.pipeline_args(root, path)
                args.sub2api_config = config
                account = remote(7, "good@example.com", managed=True)
                account["credentials"] = {}
                client = CredentialClient([account])
                with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                    sub2api, "admin_session", return_value=(config, config_value, client, "token")
                ), patch.object(sub2api, "get_accounts", return_value=list(client.accounts.values())), patch.object(
                    pipeline.cockpit_module, "execute", return_value=0
                ):
                    self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(client.updates[0][1]["credentials"]["access_token"], "test-token")

    def test_repaired_input_does_not_replay_unchanged_old_tokens(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            old = record("old@example.com")
            path.write_text(json.dumps([{"type": "codex", "email": old["email"]}]), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            _, fingerprints = sub2api.AccountConfig({}).compile([old])
            state_path = root / "cache/state/incremental.json"
            incremental.write_state(state_path, incremental.build_state(
                [old], incremental.input_scope(None, path), {"email:old@example.com": 7}, config_fingerprints=fingerprints,
            ))
            token_state.update_snapshot(root / "cache/state/token-snapshot.json", [old], {"email:old@example.com": 7})
            client = FailingClient([remote(7, "old@example.com", managed=True)])
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 1)
                path.write_text(json.dumps(old), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
            self.assertEqual(client.attempted, [])

    def test_full_pipeline_with_only_invalid_accounts_saves_failure_manifest(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps({"type": "codex", "email": "bad@example.com"}), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path, incremental_mode=False)
            args.sub2api_config = config
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "execute"
            ) as importer, patch.object(pipeline.cockpit_module, "execute") as cockpit:
                self.assertEqual(pipeline.run(args), 1)
            importer.assert_not_called()
            cockpit.assert_not_called()
            manifest = json.loads(next((root / "cache/runs").glob("*/manifest.json")).read_text(encoding="utf-8"))
            self.assertEqual(manifest["status"], "partial_failure")
            self.assertEqual(set(manifest["account_failures"]), {"email:bad@example.com"})

    def test_cockpit_failure_does_not_print_success_or_save_token_snapshot(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps(record("good@example.com")), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path, incremental_mode=False)
            args.sub2api_config = config
            client = FailingClient()
            output = io.StringIO()
            with contextlib.redirect_stdout(output), contextlib.redirect_stderr(io.StringIO()), patch.object(
                pipeline, "load_pipeline_config", return_value={}
            ), patch.object(sub2api, "admin_session", return_value=(config, {}, client, "token")), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ), patch.object(pipeline.cockpit_module, "execute", return_value=1):
                self.assertEqual(pipeline.run(args), 1)
            self.assertNotIn("[Cockpit] 已提交", output.getvalue())
            self.assertFalse((root / "cache/state/token-snapshot.json").exists())

    def test_existing_credential_retry_preserves_failed_snapshot_and_continues_successes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            records = [record("bad@example.com"), record("good@example.com")]
            path = root / "accounts.json"
            path.write_text(json.dumps(records), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            _, fingerprints = sub2api.AccountConfig({}).compile(records)
            state_path = root / "cache/state/incremental.json"
            state = incremental.build_state(records, incremental.input_scope(None, path), {
                "email:bad@example.com": 7, "email:good@example.com": 8,
            }, config_fingerprints=fingerprints)
            for item in state["accounts"].values():
                item["retry"] = True
            incremental.write_state(state_path, state)
            old_records = [{**value, "access_token": "old-token"} for value in records]
            snapshot_path = root / "cache/state/token-snapshot.json"
            token_state.update_snapshot(snapshot_path, old_records, {"email:bad@example.com": 7, "email:good@example.com": 8})
            client = FailingClient([remote(7, "bad@example.com", managed=True), remote(8, "good@example.com", managed=True)])
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 1)
                state = incremental.read_state(state_path)["accounts"]
                self.assertTrue(state["email:bad@example.com"]["retry"])
                self.assertFalse(state["email:good@example.com"].get("retry", False))
                self.assertEqual(state["email:bad@example.com"]["sub2api_id"], 7)
                self.assertEqual(token_state.read_snapshot(snapshot_path)["accounts"]["email:bad@example.com"]["access_token"], "old-token")
                client.failed_email = None
                client.attempted.clear()
                self.assertEqual(pipeline.run(args), 0)
            self.assertEqual(client.attempted, ["bad@example.com"])


if __name__ == "__main__":
    unittest.main()
