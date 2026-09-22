"""增量删除和远端归属边界的离线回归测试。"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch


sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import incremental
import run as pipeline
from normalize import main as normalize
from redeem import main as redeem
from sub2api import main as sub2api


def record(email: str) -> dict:
    return {"type": "codex", "email": email, "access_token": "test-token"}


def remote(account_id: int, email: str, *, managed: bool) -> dict:
    extra = (
        {sub2api.OWNERSHIP_EXTRA_KEY: sub2api.OWNERSHIP_EXTRA_VALUE}
        if managed
        else {}
    )
    return {"id": account_id, "email": email, "name": email, "extra": extra}


class FakeClient:
    def __init__(self, accounts: list[dict]):
        self.accounts = {account["id"]: account for account in accounts}
        self.imports: list[dict] = []
        self.deleted: list[int] = []

    def request(self, method: str, path: str, *, token=None, body=None):
        if (method, path) == ("GET", "/admin/groups/all?include_inactive=true"):
            return []
        if method == "GET" and path.startswith("/admin/accounts/"):
            return self.accounts[int(path.rsplit("/", 1)[1])]
        if (method, path) == ("POST", "/admin/accounts/import/codex-session"):
            self.imports.append(body)
            incoming = json.loads(body["content"])
            email = incoming["email"]
            existing = next(
                (account for account in self.accounts.values() if account["email"] == email),
                None,
            )
            if existing is None:
                account_id = max(self.accounts, default=0) + 1
                self.accounts[account_id] = remote(account_id, email, managed=True)
                action = "created"
            else:
                account_id = existing["id"]
                existing["extra"] = dict(body["extra"])
                action = "updated"
            return {"failed": 0, "items": [{"action": action, "account_id": account_id}]}
        if (method, path) == ("POST", "/admin/accounts/batch-delete"):
            self.deleted.extend(body["account_ids"])
            for account_id in body["account_ids"]:
                self.accounts.pop(account_id)
            return {"failed": 0, "success": len(body["account_ids"])}
        raise AssertionError((method, path))


def run_import(client: FakeClient, records: list[dict], summary: Path) -> dict:
    config = {"extra": {}}
    batch = sub2api.Batch(1, None, None, "test", "codex", [], records)
    with patch.object(sub2api, "admin_session", return_value=(summary, config, client, "token")), patch.object(
        sub2api, "build_batches", return_value=[batch]
    ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())):
        assert sub2api.run_import(None, None, summary_file=summary) == 0
    return json.loads(summary.read_text(encoding="utf-8"))["accounts"]


class IncrementalOwnershipTests(unittest.TestCase):
    @staticmethod
    def pipeline_args(root: Path, *, codes: Path | None = None, accounts: Path | None = None, incremental_mode: bool = True) -> argparse.Namespace:
        return argparse.Namespace(
            input_file=None,
            codes_file=codes,
            accounts_file=accounts,
            runtime_dir=root / "cache",
            sub2api_config=None,
            wait_seconds=60,
            skip_sub2api=False,
            skip_cockpit=False,
            incremental=incremental_mode,
            dry_run=False,
        )

    def test_commented_last_code_removes_only_previous_managed_account(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codes = root / "redeem-codes.txt"
            codes.write_text("# PLUS-OLD\n\n", encoding="utf-8")
            self.assertEqual(redeem.read_codes(codes, allow_empty=True), [])
            metadata = normalize.normalize(None, root / "normalized", allow_empty=True)
            self.assertEqual(metadata["source_keys"], {})
            current = json.loads(Path(metadata["cockpit_input"]).read_text(encoding="utf-8"))
            scope = incremental.input_scope(codes, None)
            previous = incremental.build_state(
                [record("old@example.com")],
                scope,
                {"email:old@example.com": 7},
                source_keys={"email:old@example.com": ["redeem"]},
            )
            delta = incremental.compare(current, previous, scope)
            self.assertEqual(delta.added, [])
            self.assertEqual(delta.removed[0]["sub2api_id"], 7)

    def test_empty_codes_file_does_not_trigger_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            codes = Path(directory) / "redeem-codes.txt"
            codes.write_text("\n\n", encoding="utf-8")
            with self.assertRaises(redeem.RedeemError):
                redeem.read_codes(codes, allow_empty=True)

    def test_blank_accounts_file_is_optional_source(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text("\n\t\n", encoding="utf-8")
            metadata = normalize.normalize(None, root / "out", accounts, allow_empty=True)
            self.assertEqual(metadata["accounts"], 0)
            self.assertEqual(metadata["source_keys"], {})

    def test_normalize_keeps_source_membership_without_duplicates(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            downloaded = root / "downloaded"
            downloaded.mkdir()
            downloaded.joinpath("account.json").write_text(
                json.dumps(record("same@example.com")), encoding="utf-8"
            )
            account_text = root / "accounts.txt"
            account_text.write_text(
                json.dumps(record("same@example.com"))
                + "\n\n"
                + json.dumps(record("direct@example.com")),
                encoding="utf-8",
            )
            metadata = normalize.normalize(downloaded, root / "out", account_text)
            self.assertEqual(metadata["accounts"], 2)
            self.assertEqual(
                metadata["source_keys"]["email:same@example.com"],
                ["accounts-file", "redeem"],
            )
            self.assertEqual(
                metadata["source_keys"]["email:direct@example.com"],
                ["accounts-file"],
            )

    def test_manual_account_is_never_updated_or_recorded(self):
        with tempfile.TemporaryDirectory() as directory:
            client = FakeClient([remote(7, "manual@example.com", managed=False)])
            summary = run_import(client, [record("manual@example.com")], Path(directory) / "summary.json")
            self.assertEqual(client.imports, [])
            self.assertEqual(summary, {})
            self.assertFalse(sub2api.is_tool_managed(client.accounts[7]))

    def test_new_and_existing_managed_accounts_keep_marker(self):
        with tempfile.TemporaryDirectory() as directory:
            client = FakeClient([remote(7, "existing@example.com", managed=True)])
            summary = run_import(
                client,
                [record("existing@example.com"), record("new@example.com")],
                Path(directory) / "summary.json",
            )
            self.assertEqual(summary, {"email:existing@example.com": 7, "email:new@example.com": 8})
            self.assertTrue(client.imports[0]["update_existing"])
            self.assertFalse(client.imports[1]["update_existing"])
            self.assertTrue(all(sub2api.is_tool_managed(account) for account in client.accounts.values()))

    def test_delete_requires_same_id_marker_and_email(self):
        client = FakeClient([remote(7, "old@example.com", managed=True)])
        expected = [dict(record("old@example.com"), sub2api_id=7)]
        with patch.object(sub2api, "admin_session", return_value=(Path("config"), {}, client, "token")), patch.object(
            sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
        ):
            client.accounts[7]["extra"] = {}
            with self.assertRaises(sub2api.Sub2ApiError):
                sub2api.delete_account_ids(None, [7], expected_records=expected)
            self.assertEqual(client.deleted, [])
            client.accounts[7]["extra"] = {
                sub2api.OWNERSHIP_EXTRA_KEY: sub2api.OWNERSHIP_EXTRA_VALUE
            }
            client.accounts[7]["email"] = "changed@example.com"
            client.accounts[7]["name"] = "changed@example.com"
            with self.assertRaises(sub2api.Sub2ApiError):
                sub2api.delete_account_ids(None, [7], expected_records=expected)
            self.assertEqual(client.deleted, [])
            client.accounts[7]["email"] = "old@example.com"
            client.accounts[7]["name"] = "old@example.com"
            self.assertEqual(sub2api.delete_account_ids(None, [7], expected_records=expected), 0)
            self.assertEqual(client.deleted, [7])

    def test_already_deleted_account_is_safe_to_retry(self):
        class MissingClient(FakeClient):
            def request(self, method: str, path: str, *, token=None, body=None):
                if method == "GET" and path == "/admin/accounts/7":
                    raise sub2api.Sub2ApiError("请求 Sub2API GET /admin/accounts/7 失败（HTTP 404）")
                return super().request(method, path, token=token, body=body)

        client = MissingClient([])
        with patch.object(
            sub2api,
            "admin_session",
            return_value=(Path("config"), {}, client, "token"),
        ):
            self.assertEqual(
                sub2api.delete_account_ids(
                    None,
                    [7],
                    expected_records=[dict(record("old@example.com"), sub2api_id=7)],
                ),
                0,
            )
        self.assertEqual(client.deleted, [])

    def test_v1_snapshot_cannot_drive_deletion(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "incremental.json"
            path.write_text(
                json.dumps(
                    {
                        "version": 1,
                        "scope": {"codes_file": "old"},
                        "accounts": {"email:manual@example.com": {"sub2api_id": 7}},
                    }
                ),
                encoding="utf-8",
            )
            state = incremental.read_state(path)
            delta = incremental.compare([], state, {"codes_file": "old"})
            self.assertEqual(delta.removed, [])
            self.assertIsNone(state["scope"])

    def test_pipeline_cleans_up_when_all_codes_are_commented(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codes = root / "redeem-codes.txt"
            codes.write_text("# PLUS-OLD\n", encoding="utf-8")
            runtime = root / "cache"
            state_path = runtime / "state" / "incremental.json"
            incremental.write_state(
                state_path,
                incremental.build_state(
                    [record("old@example.com")],
                    incremental.input_scope(codes, None),
                    {"email:old@example.com": 7},
                ),
            )
            client = FakeClient([remote(7, "old@example.com", managed=True)])
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                pipeline.redeem_module, "execute", side_effect=AssertionError("redeem called")
            ), patch.object(
                pipeline.cockpit_module, "execute", side_effect=AssertionError("cockpit called")
            ), patch.object(
                sub2api, "admin_session", return_value=(root, {}, client, "token")
            ), patch.object(
                sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())
            ):
                self.assertEqual(pipeline.run(self.pipeline_args(root, codes=codes)), 0)
            self.assertEqual(client.deleted, [7])
            new_state = incremental.read_state(state_path)
            self.assertEqual(new_state["accounts"], {})
            self.assertIn(
                "old@example.com",
                (runtime / "results" / "cockpit-pending-deletions.txt").read_text(encoding="utf-8-sig"),
            )

    def test_pipeline_keeps_id_still_referenced_by_current_input(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codes = root / "redeem-codes.txt"
            codes.write_text("# PLUS-OLD\n", encoding="utf-8")
            accounts = root / "accounts.txt"
            accounts.write_text(json.dumps(record("keep@example.com")), encoding="utf-8")
            state_path = root / "cache" / "state" / "incremental.json"
            incremental.write_state(
                state_path,
                incremental.build_state(
                    [record("old@example.com"), record("keep@example.com")],
                    incremental.input_scope(codes, accounts),
                    {"email:old@example.com": 7, "email:keep@example.com": 7},
                ),
            )
            account = remote(7, "keep@example.com", managed=True)
            account["name"] = "old@example.com"
            client = FakeClient([account])
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                pipeline.redeem_module, "execute", side_effect=AssertionError("redeem called")
            ), patch.object(
                pipeline.cockpit_module, "execute", side_effect=AssertionError("cockpit called")
            ), patch.object(
                sub2api, "admin_session", return_value=(root, {}, client, "token")
            ), patch.object(
                sub2api, "delete_account_ids", side_effect=AssertionError("delete called")
            ):
                self.assertEqual(
                    pipeline.run(self.pipeline_args(root, codes=codes, accounts=accounts)),
                    0,
                )
            self.assertIn(7, client.accounts)
            self.assertEqual(
                set(incremental.read_state(state_path)["accounts"]),
                {"email:keep@example.com"},
            )

    def test_pipeline_does_not_send_skipped_manual_account_to_cockpit(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text(json.dumps(record("manual@example.com")), encoding="utf-8")

            def no_owned_accounts(*_args, **kwargs):
                sub2api.write_managed_summary(kwargs["summary_file"], {})
                return 0

            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                pipeline.sub2api_module, "execute", side_effect=no_owned_accounts
            ), patch.object(
                pipeline.cockpit_module, "execute", side_effect=AssertionError("cockpit called")
            ):
                self.assertEqual(
                    pipeline.run(self.pipeline_args(root, accounts=accounts, incremental_mode=False)),
                    0,
                )


if __name__ == "__main__":
    unittest.main()
