from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from inputs import InputFiles
from normalize import main as normalize
import run as pipeline
from test_incremental_ownership import record


class InputFailureIsolationTests(unittest.TestCase):
    def test_input_errors_and_conflicts_leave_unrelated_account_importable(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps([
                record("conflict@example.com"),
                {**record("conflict@example.com"), "access_token": "secret-conflicting-token"},
                {"email": "missing-token@example.com", "type": "codex"},
                record("good@example.com"),
            ]), encoding="utf-8")
            inputs = InputFiles([], [path])
            self.assertEqual([r["email"] for r in inputs.load(allow_empty=False, conflict_file=root / "conflicts.txt")], ["good@example.com"])
            metadata = normalize.normalize(None, root / "out", record_sources=inputs.sources, failed_accounts=inputs.failures)
            self.assertEqual(metadata["accounts"], 1)
            self.assertEqual(set(metadata["failed_accounts"]), {"email:conflict@example.com", "email:missing-token@example.com"})
            self.assertNotIn("secret-conflicting-token", json.dumps(metadata))

    def test_conflicting_account_ids_quarantine_both_email_aliases(self):
        values = [
            {**record("one@example.com"), "account_id": "first-id"},
            {**record("two@example.com"), "account_id": "shared-id"},
            {**record("one@example.com"), "account_id": "shared-id"},
            record("good@example.com"),
        ]
        records, _, conflicts = normalize.deduplicate_with_sources((value, "input") for value in values)
        self.assertEqual([r["email"] for r in records], ["good@example.com"])
        self.assertEqual(set(conflicts), {"email:one@example.com", "email:two@example.com"})
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "conflicts.txt"
            normalize.write_conflict_report(path, conflicts)
            report = path.read_text(encoding="utf-8-sig")
            self.assertIn("one@example.com", report)
            self.assertIn("two@example.com", report)

    def test_normalize_isolates_invalid_download_and_account_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            data_dir = root / "data"
            data_dir.mkdir()
            (data_dir / "bad.json").write_text("{broken", encoding="utf-8")
            (data_dir / "good.json").write_text(json.dumps(record("good@example.com")), encoding="utf-8")
            account_file = root / "accounts.json"
            account_file.write_text("{broken", encoding="utf-8")
            metadata = normalize.normalize(data_dir, root / "out", account_file)
            self.assertEqual(metadata["accounts"], 1)
            self.assertEqual(len(metadata["failed_accounts"]), 2)
            self.assertEqual([r["email"] for r in pipeline.normalized_records(Path(metadata["cockpit_input"]))], ["good@example.com"])

    def test_nested_tokens_are_read_before_classifying_account_as_missing_credentials(self):
        for container in ("auth", "data", "tokens"):
            with self.subTest(container=container):
                values = normalize.records_from_values([
                    {"email": "good@example.com", container: {"access_token": "token"}},
                    {"credentials": {"email": "missing@example.com"}},
                ])
                failures = {}
                sources = normalize.canonical_sources(((value, "input") for value in values), failures)
                self.assertEqual([value["email"] for value, _ in sources], ["good@example.com"])
                self.assertEqual(set(failures), {"email:missing@example.com"})

    def test_normalized_counts_only_include_importable_accounts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            conflicting = record("bad@example.com")
            metadata = normalize.normalize(None, root, record_sources=[
                (conflicting, "accounts-file:one"),
                ({**conflicting, "access_token": "different"}, "accounts-file:two"),
                (record("good@example.com"), "accounts-file:one"),
            ])
            self.assertEqual(metadata["source_counts"]["accounts-file"], 1)
            self.assertEqual(set(metadata["source_keys"]), {"email:good@example.com"})

    def test_duplicate_json_keys_do_not_silently_select_a_token(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            bad = root / "accounts-bad.json"
            bad.write_text('{"email":"bad@example.com","access_token":"first","access_token":"second"}', encoding="utf-8")
            good = root / "accounts-good.json"
            good.write_text(json.dumps(record("good@example.com")), encoding="utf-8")
            inputs = InputFiles([], [bad, good])
            values = inputs.load(allow_empty=False, conflict_file=root / "conflicts.txt")
            self.assertEqual([r["email"] for r in values], ["good@example.com"])
            self.assertEqual(set(inputs.failures), {"source:" + str(bad)})

    def test_identityless_invalid_account_is_reported_instead_of_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "accounts.json"
            path.write_text(json.dumps([{"type": "codex"}, record("good@example.com")]), encoding="utf-8")
            inputs = InputFiles([], [path])
            values = inputs.load(allow_empty=False, conflict_file=root / "conflicts.txt")
            self.assertEqual([r["email"] for r in values], ["good@example.com"])
            self.assertEqual(len(inputs.failures), 1)
            self.assertTrue(next(iter(inputs.failures)).startswith("source:"))


if __name__ == "__main__":
    unittest.main()
