from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import incremental
import token_state
import run as pipeline
from sub2api import main as sub2api
import test_incremental_ownership as ownership
from test_incremental_ownership import record, remote
from test_sub2api_failure_isolation import FailingClient


class IncrementalTokenChangeTests(unittest.TestCase):
    def test_incremental_detects_edited_json_tokens(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            value = record("good@example.com")
            path.write_text(json.dumps(value), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            client = FailingClient(failed_email=None)
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 0)
                client.attempted.clear()
                value["access_token"] = "new-token"
                path.write_text(json.dumps(value), encoding="utf-8")
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(client.attempted, ["good@example.com"])
                client.attempted.clear()
                self.assertEqual(pipeline.run(args), 0)
                self.assertEqual(client.attempted, [])

    def test_incremental_handles_malformed_previous_token_entry_without_traceback(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            value = record("good@example.com")
            path = root / "accounts.json"
            path.write_text(json.dumps(value), encoding="utf-8")
            config = root / "config.json"
            config.write_text("{}", encoding="utf-8")
            args = ownership.IncrementalOwnershipTests.pipeline_args(root, accounts=path)
            args.sub2api_config = config
            _, fingerprints = sub2api.AccountConfig({}).compile([value])
            incremental.write_state(root / "cache/state/incremental.json", incremental.build_state(
                [value], incremental.input_scope(None, path), {"email:good@example.com": 7}, config_fingerprints=fingerprints,
            ))
            snapshot_path = root / "cache/state/token-snapshot.json"
            incremental.write_json(snapshot_path, {"version": 1, "accounts": {"email:good@example.com": None}})
            client = FailingClient([remote(7, "good@example.com", managed=True)])
            with patch.object(pipeline, "load_pipeline_config", return_value={}), patch.object(
                sub2api, "admin_session", return_value=(config, {}, client, "token")
            ), patch.object(sub2api, "get_accounts", side_effect=lambda *_: list(client.accounts.values())), patch.object(
                pipeline.cockpit_module, "execute", return_value=0
            ):
                self.assertEqual(pipeline.run(args), 0)
            self.assertEqual(client.attempted, ["good@example.com"])
            self.assertEqual(token_state.read_snapshot(snapshot_path)["accounts"]["email:good@example.com"]["access_token"], "test-token")


if __name__ == "__main__":
    unittest.main()
