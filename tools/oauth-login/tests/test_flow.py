"""协议、安全失败路径与现有 JSON 格式的离线回归验证。"""

import contextlib
import importlib.util
import io
import json
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import Mock, patch
from urllib.parse import urlencode, urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

import jwt

import main
from browser import ChromeLogin, StepPacer
from demo import LocalFixture, PUBLIC_SECRET
from flow import Authorization, CallbackServer, Credentials, JsonExporter, LoginError, LoginFlow, Provider, ProxyHttpClient, TokenValidator


class ProtocolTests(unittest.TestCase):
    def test_totp_rfc_vectors_and_window(self):
        credentials = Credentials("fixture@example.invalid", "fake-password", PUBLIC_SECRET)
        for timestamp, expected in [(59, "287082"), (1111111109, "081804"), (1111111111, "050471"), (1234567890, "005924"), (2000000000, "279037"), (20000000000, "353130")]:
            with self.subTest(timestamp=timestamp):
                code, remaining = credentials.otp(timestamp)
                self.assertEqual(code, expected)
                self.assertEqual(remaining, 30 - timestamp % 30)
        self.assertEqual(credentials.otp(29)[1], 1)
        self.assertEqual(credentials.otp(30)[1], 30)
        self.assertNotIn("fake-password", repr(credentials))
        self.assertNotIn(PUBLIC_SECRET, repr(credentials))

    def test_pkce_known_vector(self):
        authorization = Authorization(verifier="dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
        self.assertEqual(authorization.challenge, "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM")
        self.assertNotEqual(Authorization().state, Authorization().state)

    def test_callback_rejects_host_and_is_single_use(self):
        import http.client

        with CallbackServer("expected-state", "http://127.0.0.1:0/auth/callback") as callback:
            parsed = urlsplit(callback.uri)

            def get(parameters, host=None):
                connection = http.client.HTTPConnection("127.0.0.1", parsed.port, timeout=3)
                try:
                    headers = {"Host": host} if host else {}
                    connection.request("GET", parsed.path + "?" + urlencode(parameters, doseq=True), headers=headers)
                    response = connection.getresponse()
                    response.read()
                    return response.status
                finally:
                    connection.close()

            self.assertEqual(get({"state": "expected-state", "code": "first"}, "untrusted.invalid"), 404)
            self.assertFalse(callback.done.is_set())
            self.assertEqual(get({"state": "expected-state", "code": "first"}), 200)
            self.assertEqual(callback.result(), "first")
            get({"state": "expected-state", "code": "second"})
            self.assertEqual(callback.result(), "first")
        self.assertFalse(callback.thread.is_alive())

    def test_callback_rejects_bad_or_duplicate_state(self):
        import http.client

        for state in ("wrong", ["expected", "expected"]):
            with self.subTest(state=state), CallbackServer("expected", "http://127.0.0.1:0/callback") as callback:
                parsed = urlsplit(callback.uri)
                connection = http.client.HTTPConnection("127.0.0.1", parsed.port, timeout=3)
                try:
                    connection.request("GET", parsed.path + "?" + urlencode({"state": state, "code": "unused"}, doseq=True))
                    response = connection.getresponse()
                    response.read()
                    self.assertEqual(response.status, 400)
                    with self.assertRaisesRegex(LoginError, "state"):
                        callback.result()
                finally:
                    connection.close()

    def test_fixed_proxy_and_connect_target(self):
        with self.assertRaises(LoginError):
            ProxyHttpClient("http://127.0.0.1:8080")
        connection = Mock()
        connection.getresponse.return_value.status = 200
        connection.getresponse.return_value.read.return_value = b'{"ok": true}'
        with patch("flow.http.client.HTTPSConnection", return_value=connection) as constructor:
            self.assertEqual(ProxyHttpClient().request_json("https://issuer.example.invalid/token", {"code": "fake-code"}), {"ok": True})
        self.assertEqual(constructor.call_args.args, ("127.0.0.1", 7897))
        connection.set_tunnel.assert_called_once_with("issuer.example.invalid", 443)
        self.assertEqual(connection.request.call_args.args[:2], ("POST", "/token"))
        connection.close.assert_called_once()

    def test_proxy_failure_prevents_browser_start(self):
        with LocalFixture() as fixture, tempfile.TemporaryDirectory() as directory:
            browser = Mock()
            with patch("flow.socket.create_connection", side_effect=OSError("private diagnostic")):
                with self.assertRaisesRegex(LoginError, "不会回退直连") as caught:
                    LoginFlow(fixture.provider, ProxyHttpClient(), browser, report=lambda _: None).run(fixture.credentials, Path(directory) / "account.json")
            browser.authorize.assert_not_called()
            self.assertNotIn("private diagnostic", str(caught.exception))
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_http_errors_do_not_retry_or_echo_response(self):
        connection = Mock()
        connection.getresponse.return_value.status = 302
        connection.getresponse.return_value.read.return_value = b"private-token-response"
        with patch("flow.http.client.HTTPSConnection", return_value=connection):
            with self.assertRaises(LoginError) as caught:
                ProxyHttpClient().request_json("https://issuer.example.invalid/token")
        self.assertNotIn("private-token-response", str(caught.exception))
        connection.request.assert_called_once()
        connection.close.assert_called_once()

    def test_proxy_disconnect_does_not_create_direct_connection(self):
        connection = Mock()
        connection.request.side_effect = OSError("private-token-response")
        with patch("flow.http.client.HTTPSConnection", return_value=connection) as constructor:
            with self.assertRaisesRegex(LoginError, "不会回退直连"):
                ProxyHttpClient().request_json("https://issuer.example.invalid/token")
        constructor.assert_called_once()
        self.assertEqual(constructor.call_args.args, ("127.0.0.1", 7897))
        connection.close.assert_called_once()

    def test_cli_blocks_incomplete_config_and_live_headless(self):
        with patch("main.ChromeLogin") as browser, contextlib.redirect_stderr(io.StringIO()) as stderr:
            self.assertEqual(main.main(["--live", "--config", str(main.ROOT / "config.example.json")]), 1)
            browser.assert_not_called()
            with self.assertRaises(SystemExit) as caught:
                main.main(["--live", "--headless"])
            self.assertEqual(caught.exception.code, 2)
            self.assertIn("尚未配置", stderr.getvalue())
        with self.assertRaises(LoginError):
            ChromeLogin(headless=True)

    def test_cli_blocks_example_credentials_in_live_mode(self):
        with LocalFixture() as fixture, patch("main.Provider.load", return_value=fixture.provider), patch("main.ChromeLogin") as browser:
            with contextlib.redirect_stderr(io.StringIO()) as stderr:
                self.assertEqual(main.main(["--live", "--input", str(main.ROOT / "input" / "account.example.json")]), 1)
            browser.assert_not_called()
            self.assertIn("示例邮箱", stderr.getvalue())

    def test_provider_rejects_plain_http_and_unregistered_dynamic_callback(self):
        values = {"client_id": "fixture", "authorization_endpoint": "https://issuer.example.invalid/authorize", "token_endpoint": "https://issuer.example.invalid/token", "issuer": "https://issuer.example.invalid", "jwks_uri": "https://issuer.example.invalid/jwks", "redirect_uri": "http://127.0.0.1:1455/callback"}
        Provider(**values).validate()
        for name, value in [("authorization_endpoint", "http://issuer.example.invalid/authorize"), ("redirect_uri", "http://127.0.0.1:0/callback"), ("redirect_uri", "http://public.example.invalid:1455/callback")]:
            with self.subTest(name=name, value=value), self.assertRaises(LoginError):
                Provider(**{**values, name: value}).validate()

    def test_step_pacing_is_bounded(self):
        sleep, uniform = Mock(), Mock(return_value=0.8)
        StepPacer(sleep=sleep, uniform=uniform).pause()
        uniform.assert_called_once_with(0.4, 1.2)
        sleep.assert_called_once_with(0.8)


class TokenAndOutputTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fixture = LocalFixture().__enter__()

    @classmethod
    def tearDownClass(cls):
        cls.fixture.__exit__()

    def validate(self, tokens, authorization, jwks=None, requested_at=None):
        return TokenValidator().validate(
            tokens, self.fixture.jwks if jwks is None else jwks, self.fixture.provider, authorization, self.fixture.credentials,
            requested_at=time.time() if requested_at is None else requested_at,
        )

    def test_decimal_access_expiration_limits_output_expiry(self):
        authorization = Authorization()
        tokens = self.fixture.issue_tokens(authorization.nonce)
        expiration = time.time() + 120.75
        tokens["access_token"] = jwt.encode({"exp": expiration}, self.fixture.private_key, algorithm="RS256")
        self.assertEqual(self.validate(tokens, authorization)["expires_at"], int(expiration))

    def test_network_and_validation_time_do_not_extend_opaque_token_lifetime(self):
        authorization = Authorization()
        tokens = {**self.fixture.issue_tokens(authorization.nonce), "access_token": "fixture-opaque-token", "expires_in": 30}
        with self.assertRaisesRegex(LoginError, "Access Token 已过期"):
            self.validate(tokens, authorization, requested_at=time.time() - 60)

    def test_valid_output_is_accepted_by_existing_normalizer(self):
        authorization = Authorization()
        record = self.validate(self.fixture.issue_tokens(authorization.nonce), authorization)
        path = main.ROOT.parent / "account-pipeline" / "normalize" / "main.py"
        spec = importlib.util.spec_from_file_location("existing_normalizer", path)
        normalizer = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(normalizer)
        self.assertEqual(normalizer.canonical_record(record, 1), record)
        self.assertEqual(set(record), {"type", "email", "account_id", "access_token", "refresh_token", "id_token", "expires_at"})

    def test_invalid_identity_signature_nonce_or_expiry_rejected(self):
        authorization = Authorization()
        valid = self.fixture.issue_tokens(authorization.nonce)
        cases = {
            "nonce": self.fixture.issue_tokens("wrong-nonce"),
            "audience": self.fixture.issue_tokens(authorization.nonce, audience="wrong-client"),
            "email": self.fixture.issue_tokens(authorization.nonce, email="other@example.invalid"),
            "expired": self.fixture.issue_tokens(authorization.nonce, lifetime=-60),
            "missing_refresh": {**valid, "refresh_token": ""},
            "token_type": {**valid, "token_type": "unexpected"},
        }
        claims = jwt.decode(valid["id_token"], options={"verify_signature": False})
        for name, changed in [
            ("issuer", {**claims, "iss": "https://other.invalid"}),
            ("azp", {**claims, "aud": [self.fixture.provider.client_id, "other"], "azp": "other"}),
            ("missing_sub", {key: value for key, value in claims.items() if key != "sub"}),
            ("missing_email_verified", {key: value for key, value in claims.items() if key != "email_verified"}),
            ("unverified_email", {**claims, "email_verified": "false"}),
        ]:
            cases[name] = {**valid, "id_token": jwt.encode(changed, self.fixture.private_key, algorithm="RS256", headers={"kid": "local-fixture"})}
        with LocalFixture() as other:
            cases["signature"] = other.issue_tokens(authorization.nonce)
        for name, tokens in cases.items():
            with self.subTest(name=name), self.assertRaises(LoginError):
                self.validate(tokens, authorization)

    def test_invalid_lifetime_and_jwks_rejected(self):
        authorization = Authorization()
        valid = self.fixture.issue_tokens(authorization.nonce)
        for lifetime in (0, -1, True, float("nan"), float("inf"), "1800"):
            with self.subTest(lifetime=lifetime), self.assertRaises(LoginError):
                self.validate({**valid, "expires_in": lifetime}, authorization)
        for jwks in ({}, {"keys": "wrong"}, {"keys": [None]}, {"keys": []}):
            with self.subTest(jwks=jwks), self.assertRaises(LoginError):
                self.validate(valid, authorization, jwks=jwks)

    def test_atomic_write_preserves_other_account_and_failed_replacement(self):
        authorization = Authorization()
        record = self.validate(self.fixture.issue_tokens(authorization.nonce), authorization)
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "account.json"
            output.write_text(json.dumps({"email": "other@example.invalid", "marker": "preserve"}), encoding="utf-8")
            before = output.read_bytes()
            with self.assertRaises(LoginError):
                JsonExporter().write(record, output)
            self.assertEqual(output.read_bytes(), before)
            output.write_text(json.dumps({"email": record["email"], "marker": "preserve"}), encoding="utf-8")
            before = output.read_bytes()
            with patch("flow.os.replace", side_effect=OSError("private diagnostic")), self.assertRaises(LoginError):
                JsonExporter().write(record, output)
            self.assertEqual(output.read_bytes(), before)
            self.assertEqual(list(Path(directory).glob(".oauth-*")), [])
            JsonExporter().write(record, output)
            self.assertEqual(json.loads(output.read_text(encoding="utf-8")), record)

    def test_failed_token_validation_preserves_existing_output(self):
        transport, browser, messages = Mock(), Mock(), []
        transport.request_json.side_effect = [self.fixture.issue_tokens("wrong-nonce"), self.fixture.jwks]

        def complete_callback(url, credentials, callback, client):
            callback.code = "fake-code"
            callback.done.set()

        browser.authorize.side_effect = complete_callback
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "account.json"
            output.write_text('{"marker": "preserve old authorization"}', encoding="utf-8")
            before = output.read_bytes()
            with self.assertRaisesRegex(LoginError, "nonce"):
                LoginFlow(self.fixture.provider, transport, browser, report=messages.append).run(self.fixture.credentials, output)
            self.assertEqual(output.read_bytes(), before)
            self.assertFalse(any("[5/5]" in message for message in messages))


if __name__ == "__main__":
    unittest.main()
