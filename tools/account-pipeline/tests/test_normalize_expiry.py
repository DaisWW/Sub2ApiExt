"""标准化账号到期时间的回归测试。"""

from __future__ import annotations

import base64
import json
import sys
import unittest
from pathlib import Path


sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from normalize import main as normalize
from sub2api import main as sub2api


def jwt_with_claims(claims: dict) -> str:
    header = {"alg": "none", "typ": "JWT"}

    def encode(value: dict) -> str:
        return base64.urlsafe_b64encode(
            json.dumps(value, separators=(",", ":")).encode("utf-8")
        ).decode("ascii").rstrip("=")

    return f"{encode(header)}.{encode(claims)}.signature"


class NormalizeExpiryTests(unittest.TestCase):
    def test_jwt_exp_replaces_stale_exported_expiration(self):
        record = {
            "type": "codex",
            "email": "one@example.com",
            "access_token": jwt_with_claims(
                {"email": "one@example.com", "exp": 200}
            ),
            "expires_at": 100,
        }

        normalized = normalize.canonical_record(record, 1)

        self.assertEqual(normalized["expires_at"], 200)

    def test_non_jwt_tokens_keep_legacy_expiration(self):
        record = {
            "type": "codex",
            "email": "one@example.com",
            "access_token": "opaque-token",
            "expires_at": 100,
        }

        normalized = normalize.canonical_record(record, 1)

        self.assertEqual(normalized["expires_at"], 100)

    def test_failed_import_message_is_visible_without_token_content(self):
        token = "eyJhbGciOiJub25lIn0.eyJleHAiOjIwMH0.signature"
        result = {
            "items": [
                {
                    "index": 4,
                    "message": f"access_token: {token}",
                }
            ]
        }

        message = sub2api.failed_item_message(result, 4)

        self.assertEqual(message, "access_token: <已隐藏>")
        self.assertNotIn(token, message)


if __name__ == "__main__":
    unittest.main()
