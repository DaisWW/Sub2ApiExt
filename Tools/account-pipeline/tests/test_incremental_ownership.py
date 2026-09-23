"""增量导入、手动清单和远端归属边界的离线回归测试。"""

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

    def test_commented_last_code_is_recorded_for_manual_cleanup(self):
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

    def test_authorization_refresh_is_an_import_delta(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codes = root / "redeem-codes.txt"
            scope = incremental.input_scope(codes, None)
            existing = record("existing@example.com")
            new = record("new@example.com")
            state = incremental.build_state(
                [existing], scope, {"email:existing@example.com": 7}
            )
            delta = incremental.compare(
                [existing, new],
                state,
                scope,
                refresh_keys=["email:existing@example.com"],
            )
            self.assertEqual([item["email"] for item in delta.added], ["new@example.com"])
            self.assertEqual(
                [item["email"] for item in delta.refreshed], ["existing@example.com"]
            )
            self.assertEqual(delta.unchanged, 0)
            paths = incremental.write_delta(root / "incremental", delta)
            sub2api = json.loads(Path(paths["sub2api_input"]).read_text(encoding="utf-8"))
            cockpit = json.loads(Path(paths["cockpit_input"]).read_text(encoding="utf-8"))
            self.assertEqual(
                [item["credentials"]["email"] for item in sub2api["accounts"]],
                ["new@example.com", "existing@example.com"],
            )
            self.assertEqual(
                [item["email"] for item in cockpit],
                ["new@example.com", "existing@example.com"],
            )

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
                metadata["source_counts"],
                {"redeem": 1, "accounts-file": 2, "overlap": 1},
            )
            self.assertEqual(
                metadata["source_keys"]["email:same@example.com"],
                ["accounts-file", "redeem"],
            )
            self.assertEqual(
                metadata["source_keys"]["email:direct@example.com"],
                ["accounts-file"],
            )

    def test_partial_redeem_merges_sources_and_records_cleanup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codes = root / "redeem-codes.txt"
            codes.write_text("PLUS-VALID\nPLUS-EXPIRED\n", encoding="utf-8")
            accounts = root / "accounts.txt"
            accounts.write_text(json.dumps(record("direct@example.com")), encoding="utf-8")
            scope = incremental.input_scope(codes, accounts)
            state_path = root / "cache" / "state" / "incremental.json"
            incremental.write_state(
                state_path,
                incremental.build_state(
                    [record("old@example.com")],
                    scope,
                    {"email:old@example.com": 7},
                    source_keys={"email:old@example.com": ["redeem"]},
                ),
            )
            imported: list[str] = []
            cockpit_imported: list[str] = []

            def fake_redeem(_input, **kwargs):
                data_dir = kwargs["run_dir"] / "data"
                data_dir.mkdir(parents=True)
                data_dir.joinpath("redeemed.json").write_text(
                    json.dumps(record("redeemed@example.com")), encoding="utf-8"
                )
                manifest_file = kwargs["manifest_file"]
                manifest_file.parent.mkdir(parents=True, exist_ok=True)
                manifest_file.write_text(
                    json.dumps(
                        {
                            "data_dir": str(data_dir),
                            "total": 2,
                            "success": 1,
                            "failed": 1,
                        }
                    ),
                    encoding="utf-8",
                )
                return 2

            def fake_sub2api(input_path, _config, *, summary_file=None, **_kwargs):
                payload = json.loads(input_path.read_text(encoding="utf-8"))
                imported.extend(
                    item["credentials"]["email"] for item in payload["accounts"]
                )
                sub2api.write_managed_summary(
                    summary_file,
                    {
                        "email:redeemed@example.com": 8,
                        "email:direct@example.com": 9,
                    },
                )
                return 0

            def fake_cockpit(input_path, **_kwargs):
                cockpit_imported.extend(
                    item["email"]
                    for item in json.loads(input_path.read_text(encoding="utf-8"))
                )
                return 0

            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                pipeline.redeem_module, "execute", side_effect=fake_redeem
            ), patch.object(
                pipeline.sub2api_module, "execute", side_effect=fake_sub2api
            ), patch.object(
                pipeline.cockpit_module, "execute", side_effect=fake_cockpit
            ):
                self.assertEqual(
                    pipeline.run(self.pipeline_args(root, codes=codes, accounts=accounts)),
                    0,
                )

            self.assertEqual(
                set(imported), {"redeemed@example.com", "direct@example.com"}
            )
            self.assertEqual(set(cockpit_imported), set(imported))
            state = incremental.read_state(state_path)
            self.assertEqual(
                set(state["accounts"]),
                {
                    "email:redeemed@example.com",
                    "email:direct@example.com",
                },
            )
            cockpit_pending = root / "cache" / "results" / "cockpit-pending-deletions.txt"
            self.assertIn("old@example.com", cockpit_pending.read_text(encoding="utf-8-sig"))
            sub2api_pending = root / "cache" / "results" / "sub2api-pending-deletions.txt"
            self.assertIn("old@example.com", sub2api_pending.read_text(encoding="utf-8-sig"))

    def test_incremental_authorization_refresh_imports_both_targets(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            codes = root / "redeem-codes.txt"
            codes.write_text("PLUS-REFRESH\n", encoding="utf-8")
            existing = record("existing@example.com")
            scope = incremental.input_scope(codes, None)
            state_path = root / "cache" / "state" / "incremental.json"
            incremental.write_state(
                state_path,
                incremental.build_state(
                    [existing],
                    scope,
                    {"email:existing@example.com": 7},
                    source_keys={"email:existing@example.com": ["redeem"]},
                ),
            )
            imported: list[str] = []
            cockpit_imported: list[str] = []

            def fake_redeem(_input, **kwargs):
                data_dir = kwargs["run_dir"] / "data"
                data_dir.mkdir(parents=True)
                data_dir.joinpath("refreshed.json").write_text(
                    json.dumps(existing), encoding="utf-8"
                )
                manifest_file = kwargs["manifest_file"]
                manifest_file.parent.mkdir(parents=True, exist_ok=True)
                manifest_file.write_text(
                    json.dumps(
                        {
                            "data_dir": str(data_dir),
                            "total": 1,
                            "success": 1,
                            "failed": 0,
                            "refresh_accounts": ["email:existing@example.com"],
                        }
                    ),
                    encoding="utf-8",
                )
                return 0

            def fake_sub2api(input_path, _config, *, summary_file=None, **_kwargs):
                payload = json.loads(input_path.read_text(encoding="utf-8"))
                imported.extend(
                    item["credentials"]["email"] for item in payload["accounts"]
                )
                sub2api.write_managed_summary(
                    summary_file, {"email:existing@example.com": 7}
                )
                return 0

            def fake_cockpit(input_path, **_kwargs):
                cockpit_imported.extend(
                    item["email"]
                    for item in json.loads(input_path.read_text(encoding="utf-8"))
                )
                return 0

            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                pipeline.redeem_module, "execute", side_effect=fake_redeem
            ), patch.object(
                pipeline.sub2api_module, "execute", side_effect=fake_sub2api
            ), patch.object(
                pipeline.cockpit_module, "execute", side_effect=fake_cockpit
            ):
                self.assertEqual(
                    pipeline.run(self.pipeline_args(root, codes=codes)),
                    0,
                )

            self.assertEqual(imported, ["existing@example.com"])
            self.assertEqual(cockpit_imported, ["existing@example.com"])
            manifests = list((root / "cache" / "runs").glob("*/manifest.json"))
            self.assertEqual(len(manifests), 1)
            manifest = json.loads(manifests[0].read_text(encoding="utf-8"))
            self.assertEqual(manifest["incremental_delta"]["refreshed"], 1)
            self.assertEqual(manifest["incremental_delta"]["unchanged"], 0)

    def test_full_run_imports_all_standardized_accounts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            accounts = root / "accounts.txt"
            accounts.write_text(
                json.dumps(record("first@example.com"))
                + "\n"
                + json.dumps(record("second@example.com")),
                encoding="utf-8",
            )
            incremental.write_state(
                root / "cache" / "state" / "incremental.json",
                incremental.build_state(
                    [record("first@example.com"), record("second@example.com")],
                    incremental.input_scope(None, accounts),
                    {"email:first@example.com": 7, "email:second@example.com": 8},
                ),
            )
            imported: list[str] = []
            cockpit_imported: list[str] = []

            def fake_sub2api(input_path, _config, *, summary_file=None, **_kwargs):
                payload = json.loads(input_path.read_text(encoding="utf-8"))
                imported.extend(
                    item["credentials"]["email"] for item in payload["accounts"]
                )
                sub2api.write_managed_summary(
                    summary_file,
                    {
                        "email:first@example.com": 7,
                        "email:second@example.com": 8,
                    },
                )
                return 0

            def fake_cockpit(input_path, **_kwargs):
                cockpit_imported.extend(
                    item["email"]
                    for item in json.loads(input_path.read_text(encoding="utf-8"))
                )
                return 0

            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                pipeline.sub2api_module, "execute", side_effect=fake_sub2api
            ), patch.object(
                pipeline.cockpit_module, "execute", side_effect=fake_cockpit
            ):
                self.assertEqual(
                    pipeline.run(
                        self.pipeline_args(
                            root, accounts=accounts, incremental_mode=False
                        )
                    ),
                    0,
                )

            self.assertEqual(
                imported, ["first@example.com", "second@example.com"]
            )
            self.assertEqual(
                cockpit_imported, ["first@example.com", "second@example.com"]
            )

    def test_manual_cleanup_list_persists_until_account_returns(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "sub2api-pending-deletions.txt"
            removed = [dict(record("old@example.com"), sub2api_id=7)]
            self.assertEqual(
                incremental.update_sub2api_pending_deletions(
                    path,
                    [(removed[0], "当前输入已移除；请手动处理")],
                ),
                1,
            )
            self.assertEqual(
                incremental.update_sub2api_pending_deletions(path, []),
                1,
            )
            self.assertEqual(
                incremental.update_sub2api_pending_deletions(
                    path,
                    [],
                    [record("old@example.com")],
                ),
                0,
            )
            self.assertNotIn("old@example.com", path.read_text(encoding="utf-8-sig"))

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

    def test_v1_snapshot_starts_a_safe_baseline(self):
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

    def test_pipeline_records_removed_accounts_for_manual_cleanup(self):
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
            ):
                self.assertEqual(pipeline.run(self.pipeline_args(root, codes=codes)), 0)
            self.assertIn(7, client.accounts)
            new_state = incremental.read_state(state_path)
            self.assertEqual(new_state["accounts"], {})
            self.assertIn(
                "old@example.com",
                (runtime / "results" / "cockpit-pending-deletions.txt").read_text(encoding="utf-8-sig"),
            )
            self.assertIn(
                "old@example.com",
                (runtime / "results" / "sub2api-pending-deletions.txt").read_text(encoding="utf-8-sig"),
            )
            manifests = list((runtime / "runs").glob("*/manifest.json"))
            self.assertEqual(len(manifests), 1)
            manifest = json.loads(manifests[0].read_text(encoding="utf-8"))
            self.assertEqual(manifest["manual_cleanup"]["detected"], 1)

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
                sub2api, "admin_session", side_effect=AssertionError("remote lookup called")
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
