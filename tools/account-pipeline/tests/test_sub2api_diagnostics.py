from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sub2api import main as sub2api


class Sub2ApiDiagnosticTests(unittest.TestCase):
    def test_failure_messages_redact_quoted_secrets_bearer_headers_and_proxy_credentials(self):
        for message, secrets in (
            ('{"password": "short password", "refresh_token": "short-refresh"}', ["short password", "short-refresh"]),
            ("Authorization: Bearer short-bearer", ["short-bearer"]),
            ("proxy http://proxy-user:proxy-pass@127.0.0.1:7897 failed", ["proxy-user", "proxy-pass"]),
        ):
            with self.subTest(message=message):
                safe = sub2api.safe_api_message(message)
                for secret in secrets:
                    self.assertNotIn(secret, safe)


if __name__ == "__main__":
    unittest.main()
