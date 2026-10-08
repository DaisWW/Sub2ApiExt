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
from sub2api import main as sub2api
from test_incremental_ownership import FakeClient, record, remote
from test_token_refresh import CredentialClient


class FailingClient(FakeClient):
    def __init__(self, accounts=(), *, failed_email="bad@example.com", failed_detail_id=None):
        super().__init__(list(accounts))
        self.failed_email = failed_email
        self.failed_detail_id = failed_detail_id
        self.attempted = []

    def request(self, method, path, *, token=None, body=None):
        if method == "GET" and path == f"/admin/accounts/{self.failed_detail_id}":
            raise sub2api.Sub2ApiError("账户明细查询失败")
        if (method, path) == ("POST", "/admin/accounts/import/codex-session"):
            email = json.loads(body["content"])["email"]
            self.attempted.append(email)
            if email == self.failed_email:
                return {"failed": 1, "items": [{"index": 1, "error": "access_token 已过期"}]}
        return super().request(method, path, token=token, body=body)


class Sub2ApiFailureIsolationTests(unittest.TestCase):
    def test_import_continues_after_rejected_account_and_saves_only_successes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            records = [record(email) for email in ("first@example.com", "bad@example.com", "last@example.com")]
            client = FailingClient()
            batch = sub2api.Batch(1, None, None, "test", "codex", [], records)
            summary = root / "summary.json"
            output = io.StringIO()
            with patch.object(sub2api, "admin_session", return_value=(root / "config.json", {}, client, "token")), patch.object(
                sub2api, "build_batches", return_value=[batch]
            ), patch.object(sub2api, "get_accounts", return_value=[]), contextlib.redirect_stdout(output):
                self.assertEqual(sub2api.run_import(None, None, summary_file=summary), 1)
            self.assertEqual(client.attempted, [r["email"] for r in records])
            result = json.loads(summary.read_text(encoding="utf-8"))
            self.assertEqual(set(result["accounts"]), {"email:first@example.com", "email:last@example.com"})
            self.assertEqual(set(result["failed_accounts"]), {"email:bad@example.com"})
            self.assertIn("access_token 已过期", output.getvalue())

    def test_existing_account_detail_failure_does_not_block_other_accounts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            client = FailingClient([remote(7, "bad@example.com", managed=True)], failed_detail_id=7)
            batch = sub2api.Batch(1, None, None, "test", "codex", [], [record("bad@example.com"), record("good@example.com")])
            summary = root / "summary.json"
            with patch.object(sub2api, "admin_session", return_value=(root / "config.json", {}, client, "token")), patch.object(
                sub2api, "build_batches", return_value=[batch]
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())):
                self.assertEqual(sub2api.run_import(None, None, summary_file=summary), 1)
            result = json.loads(summary.read_text(encoding="utf-8"))
            self.assertEqual(set(result["accounts"]), {"email:good@example.com"})
            self.assertEqual(client.attempted, ["good@example.com"])

    def test_token_refresh_detail_failure_does_not_block_later_account(self):
        class Client(CredentialClient):
            def request(self, method, path, *, token=None, body=None):
                if method == "GET" and path == "/admin/accounts/7":
                    raise sub2api.Sub2ApiError("账户明细查询失败")
                return super().request(method, path, token=token, body=body)

        client = Client([remote(7, "bad@example.com", managed=True), remote(8, "good@example.com", managed=True)])
        for account in client.accounts.values():
            account["credentials"] = {}
        with patch.object(sub2api, "admin_session", return_value=(Path("config.json"), {}, client, "token")), patch.object(
            sub2api, "get_accounts", return_value=list(client.accounts.values())
        ):
            result = sub2api.refresh_credentials(None, [record("bad@example.com"), record("good@example.com")])
        self.assertEqual(result.updated, {"email:good@example.com": 8})
        self.assertEqual(result.failed, ["bad@example.com"])

    def test_account_configuration_errors_are_isolated_before_import(self):
        for override in ({"concurrency": 0}, {"extra": []}, {"sub2api_url": "http://elsewhere"},
                         {"concurrency": float("inf")}, {"rate_multiplier": 10 ** 400}):
            with self.subTest(override=override):
                policy = sub2api.AccountConfig({"accounts": {"bad@example.com": override}})
                failures = {}
                configs, _ = policy.compile([record("bad@example.com"), record("good@example.com")], failed_accounts=failures)
                self.assertEqual(set(configs), {"email:good@example.com"})
                self.assertEqual(set(failures), {"email:bad@example.com"})
        policy = sub2api.AccountConfig({"redeem_codes": {"PRIVATE-CARD": {"priority": 0}}})
        failures = {}
        configs, _ = policy.compile(
            [record("bad@example.com"), record("good@example.com")], active_codes=["PRIVATE-CARD"],
            code_accounts={sub2api.hashlib.sha256(b"PRIVATE-CARD").hexdigest(): "bad@example.com"}, failed_accounts=failures,
        )
        self.assertEqual(set(configs), {"email:good@example.com"})
        self.assertNotIn("PRIVATE-CARD", json.dumps(failures))

    def test_post_import_settings_failure_is_not_recorded_as_success(self):
        class Client(FailingClient):
            def request(self, method, path, *, token=None, body=None):
                if (method, path) == ("PUT", "/admin/accounts/7"):
                    raise sub2api.Sub2ApiError("refresh_token=private-secret 更新失败")
                return super().request(method, path, token=token, body=body)

        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            client = Client([remote(7, "bad@example.com", managed=True)], failed_email=None)
            batch = sub2api.Batch(1, None, None, "test", "codex", [], [record("bad@example.com"), record("good@example.com")])
            summary = root / "summary.json"
            output = io.StringIO()
            with patch.object(sub2api, "admin_session", return_value=(root / "config.json", {}, client, "token")), patch.object(
                sub2api, "build_batches", return_value=[batch]
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), contextlib.redirect_stdout(output):
                self.assertEqual(sub2api.run_import(None, None, summary_file=summary), 1)
            result = json.loads(summary.read_text(encoding="utf-8"))
            self.assertEqual(set(result["accounts"]), {"email:good@example.com"})
            self.assertNotIn("private-secret", summary.read_text(encoding="utf-8") + output.getvalue())

    def test_card_binding_to_invalid_account_does_not_block_valid_accounts(self):
        code = "SECRET-CARD"
        policy = sub2api.AccountConfig({"redeem_codes": {code: {"priority": 80}}})
        for failures in ({}, {"email:bad@example.com": "缺少 access_token"}):
            with self.subTest(failures=failures):
                configs, _ = policy.compile(
                    [record("good@example.com")], active_codes=[code],
                    code_accounts={sub2api.hashlib.sha256(code.encode()).hexdigest(): "bad@example.com"},
                    failed_accounts=failures,
                )
                self.assertEqual(set(configs), {"email:good@example.com"})
                self.assertEqual(set(failures), {"email:bad@example.com"})
                self.assertNotIn(code, json.dumps(failures))

    def test_invalid_embedded_account_settings_do_not_block_direct_import(self):
        for invalid in ({"config": []}, {"config": {"extra": []}}, {"settings_only": "false"},
                        {"email": "bad@example.com", "credentials": []}):
            with self.subTest(invalid=invalid), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                path = root / "sub2api.json"
                path.write_text(json.dumps({"accounts": [
                    {"credentials": record("bad@example.com"), **invalid},
                    {"credentials": record("good@example.com")},
                ]}), encoding="utf-8")
                client = FailingClient()
                summary = root / "summary.json"
                with patch.object(sub2api, "admin_session", return_value=(root / "config.json", {}, client, "token")), patch.object(
                    sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
                ):
                    self.assertEqual(sub2api.run_import(path, None, summary_file=summary), 1)
                result = json.loads(summary.read_text(encoding="utf-8"))
                self.assertEqual(set(result["accounts"]), {"email:good@example.com"})
                self.assertEqual(set(result["failed_accounts"]), {"email:bad@example.com"})
                self.assertEqual(client.attempted, ["good@example.com"])

    def test_account_id_match_with_different_email_is_rejected_before_writing(self):
        bad = {**record("bad@example.com"), "account_id": "shared-id"}
        existing = remote(7, "other@example.com", managed=True)
        existing["credentials"] = {"account_id": "shared-id"}
        for token_only in (False, True):
            with self.subTest(token_only=token_only), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                good = remote(8, "good@example.com", managed=True)
                good["credentials"] = {}
                client = CredentialClient([existing, good]) if token_only else FailingClient([existing, good], failed_email=None)
                with patch.object(sub2api, "admin_session", return_value=(root / "config.json", {}, client, "token")), patch.object(
                    sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
                ):
                    if token_only:
                        result = sub2api.refresh_credentials(None, [bad, record("good@example.com")])
                        self.assertEqual(result.updated, {"email:good@example.com": 8})
                        self.assertEqual([body["account_ids"] for _, body in client.updates], [[8]])
                    else:
                        batch = sub2api.Batch(1, None, None, "test", "codex", [], [bad, record("good@example.com")])
                        with patch.object(sub2api, "build_batches", return_value=[batch]):
                            self.assertEqual(sub2api.run_import(None, None, summary_file=root / "summary.json"), 1)
                        self.assertEqual(client.attempted, ["good@example.com"])

    def test_token_update_not_applied_is_failed_without_advancing_success_mapping(self):
        class Client(CredentialClient):
            def request(self, method, path, *, token=None, body=None):
                if (method, path) == ("POST", "/admin/accounts/bulk-update") and body["account_ids"] == [7]:
                    return {"success": 1, "failed": 0}
                return super().request(method, path, token=token, body=body)

        accounts = [remote(7, "bad@example.com", managed=True), remote(8, "good@example.com", managed=True)]
        for account in accounts:
            account["credentials"] = {"access_token": "server-old"}
        client = Client(accounts)
        with patch.object(sub2api, "admin_session", return_value=(Path("config.json"), {}, client, "token")), patch.object(
            sub2api, "get_accounts", return_value=accounts
        ):
            result = sub2api.refresh_credentials(None, [record("bad@example.com"), record("good@example.com")])
        self.assertEqual(result.updated, {"email:good@example.com": 8})
        self.assertEqual(result.failed, ["bad@example.com"])


if __name__ == "__main__":
    unittest.main()
