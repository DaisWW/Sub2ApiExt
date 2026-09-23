"""明文 token 快照和凭据专用更新的离线回归测试。"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sub2api import main as sub2api
import token_state
import run as pipeline


def record(email: str, access_token: str, refresh_token: str = "refresh") -> dict:
    return {
        "type": "codex",
        "email": email,
        "access_token": access_token,
        "refresh_token": refresh_token,
        "id_token": "identity",
    }


def remote(account_id: int, email: str, *, managed: bool = True) -> dict:
    extra = (
        {sub2api.OWNERSHIP_EXTRA_KEY: sub2api.OWNERSHIP_EXTRA_VALUE}
        if managed
        else {}
    )
    return {
        "id": account_id,
        "email": email,
        "name": email,
        "platform": "openai",
        "type": "oauth",
        "credentials": {"email": email, "base_url": "https://example.invalid"},
        "extra": extra,
    }


class CredentialClient:
    def __init__(self, accounts: list[dict]):
        self.accounts = {account["id"]: account for account in accounts}
        self.updates: list[tuple[str, dict]] = []

    def request(self, method: str, path: str, *, token=None, body=None):
        if method == "GET" and path.startswith("/admin/accounts/"):
            return self.accounts[int(path.rsplit("/", 1)[1])]
        if (method, path) == ("POST", "/admin/accounts/bulk-update"):
            account_id = body["account_ids"][0]
            self.updates.append((path, body))
            self.accounts[account_id]["credentials"].update(body["credentials"])
            return {
                "success": 1,
                "failed": 0,
                "success_ids": [account_id],
                "failed_ids": [],
            }
        raise AssertionError((method, path, body))


class TokenRefreshTests(unittest.TestCase):
    @staticmethod
    def pipeline_args(root: Path, accounts: Path) -> argparse.Namespace:
        return argparse.Namespace(
            input_file=None,
            codes_file=None,
            accounts_file=accounts,
            runtime_dir=root / "cache",
            sub2api_config=None,
            wait_seconds=60,
            skip_sub2api=False,
            skip_cockpit=False,
            incremental=False,
            refresh_tokens=True,
            dry_run=False,
        )

    def test_snapshot_stores_plain_tokens_and_detects_only_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "token-snapshot.json"
            original = record("one@example.com", "plain-access")
            original_records = [original]
            token_state.update_snapshot(
                path, original_records, {"email:one@example.com": 7}
            )

            raw = path.read_text(encoding="utf-8")
            self.assertIn("plain-access", raw)
            self.assertIn("refresh", raw)
            self.assertNotIn("sha", raw.casefold())
            snapshot = token_state.read_snapshot(path)
            self.assertEqual(token_state.compare(original_records, snapshot).unchanged, 1)

            changed = record("one@example.com", "new-plain-access")
            delta = token_state.compare([changed], snapshot)
            self.assertEqual([item["email"] for item in delta.changed], ["one@example.com"])
            self.assertEqual(delta.unchanged, 0)

    def test_credential_refresh_sends_no_account_settings(self):
        with tempfile.TemporaryDirectory() as directory:
            summary = Path(directory) / "summary.json"
            account = remote(7, "one@example.com")
            client = CredentialClient([account])
            incoming = record("one@example.com", "new-access", "new-refresh")
            with patch.object(
                sub2api,
                "admin_session",
                return_value=(summary, {}, client, "admin-token"),
            ), patch.object(
                sub2api,
                "get_accounts",
                return_value=list(client.accounts.values()),
            ):
                result = sub2api.refresh_credentials(
                    None, [incoming], summary_file=summary
                )

            self.assertEqual(result.updated, {"email:one@example.com": 7})
            self.assertEqual(len(client.updates), 1)
            path, payload = client.updates[0]
            self.assertEqual(path, "/admin/accounts/bulk-update")
            self.assertEqual(set(payload), {"account_ids", "credentials"})
            self.assertEqual(payload["account_ids"], [7])
            self.assertEqual(payload["credentials"]["access_token"], "new-access")
            self.assertEqual(payload["credentials"]["refresh_token"], "new-refresh")
            self.assertNotIn("base_url", payload["credentials"])
            for forbidden in (
                "proxy_id",
                "concurrency",
                "priority",
                "rate_multiplier",
                "group_ids",
                "extra",
            ):
                self.assertNotIn(forbidden, payload)

    def test_credential_refresh_skips_manual_account(self):
        account = remote(7, "manual@example.com", managed=False)
        client = CredentialClient([account])
        with patch.object(
            sub2api,
            "admin_session",
            return_value=(Path("config.json"), {}, client, "admin-token"),
        ), patch.object(
            sub2api, "get_accounts", return_value=list(client.accounts.values())
        ):
            result = sub2api.refresh_credentials(
                None, [record("manual@example.com", "new-access")]
            )
        self.assertEqual(result.manual, ["manual@example.com"])
        self.assertEqual(client.updates, [])

    def test_refresh_pipeline_updates_changed_tokens_once(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text(
                json.dumps(record("one@example.com", "plain-access")),
                encoding="utf-8",
            )
            submitted: list[str] = []

            def fake_refresh(_config, records, *, summary_file=None):
                values = list(records)
                updated = {token_state.account_key(item): 7 for item in values}
                sub2api.write_managed_summary(summary_file, updated)
                return sub2api.CredentialRefreshResult(updated=updated)

            def fake_cockpit(input_path, **_kwargs):
                submitted.extend(
                    item["email"]
                    for item in json.loads(input_path.read_text(encoding="utf-8"))
                )
                return 0

            with patch.object(
                pipeline, "load_pipeline_config", return_value={}
            ), patch.object(
                pipeline.sub2api_module,
                "refresh_credentials",
                side_effect=fake_refresh,
            ), patch.object(
                pipeline.cockpit_module, "execute", side_effect=fake_cockpit
            ):
                self.assertEqual(
                    pipeline.run(self.pipeline_args(root, accounts)), 0
                )

            self.assertEqual(submitted, ["one@example.com"])
            snapshot_path = root / "cache" / "state" / "token-snapshot.json"
            self.assertIn("plain-access", snapshot_path.read_text(encoding="utf-8"))
            manifests = list((root / "cache" / "runs").glob("*/manifest.json"))
            self.assertEqual(len(manifests), 1)
            self.assertNotIn(
                "plain-access", manifests[0].read_text(encoding="utf-8")
            )

            with patch.object(
                pipeline, "load_pipeline_config", return_value={}
            ), patch.object(
                pipeline.sub2api_module,
                "refresh_credentials",
                side_effect=AssertionError("unchanged token was updated"),
            ), patch.object(
                pipeline.cockpit_module,
                "execute",
                side_effect=AssertionError("unchanged token reached Cockpit"),
            ):
                self.assertEqual(
                    pipeline.run(self.pipeline_args(root, accounts)), 0
                )


if __name__ == "__main__":
    unittest.main()
