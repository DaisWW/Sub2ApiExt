"""真实 Chrome + 本机虚构 OAuth/代理；不使用真实账户或公网服务。"""

import contextlib
import io
import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from browser import ChromeLogin, StepPacer
from demo import LocalFixture
from flow import LoginError, LoginFlow


def no_wait():
    return StepPacer(sleep=lambda _: None)


class BrowserTests(unittest.TestCase):
    def test_html_default_submit_button_completes_login(self):
        with LocalFixture("default_submit") as fixture, tempfile.TemporaryDirectory() as directory:
            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), report=lambda _: None, timeout=30)
            LoginFlow(fixture.provider, fixture.transport, browser, report=lambda _: None).run(fixture.credentials, Path(directory) / "account.json")
            fixture.assert_complete()
            self.assertEqual(browser.states, ["email", "password", "totp", "consent"])

    def test_normal_login_every_step_and_proxy_path(self):
        with LocalFixture() as fixture, tempfile.TemporaryDirectory() as directory, contextlib.redirect_stdout(io.StringIO()) as stdout:
            output = Path(directory) / "account.json"
            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), timeout=30)
            record = LoginFlow(fixture.provider, fixture.transport, browser).run(fixture.credentials, output)
            fixture.assert_complete()
            self.assertEqual(browser.states, ["email", "password", "totp", "consent"])
            self.assertEqual(json.loads(output.read_text(encoding="utf-8")), record)
            for path in ("/authorize", "/email", "/password", "/totp", "/consent", "/token", "/jwks"):
                self.assertIn(path, fixture.proxy_paths)
            for value in (fixture.credentials.password, fixture.credentials.totp_secret, *[record[name] for name in ("access_token", "refresh_token", "id_token")]):
                self.assertNotIn(value, stdout.getvalue())

    def test_extra_challenge_requires_manual_handoff(self):
        calls, messages = [], []

        def manual(page, state):
            calls.append(state)
            if state == "challenge":
                # This button only exists on our loopback fixture; it is not a real CAPTCHA.
                page.get_by_role("button", name="模拟人工继续").click()

        with LocalFixture("challenge") as fixture, tempfile.TemporaryDirectory() as directory:
            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), manual_handler=manual, report=messages.append, timeout=30)
            LoginFlow(fixture.provider, fixture.transport, browser, report=lambda _: None).run(fixture.credentials, Path(directory) / "account.json")
            fixture.assert_complete()
            self.assertEqual(calls, ["challenge"])
            self.assertIn("challenge", browser.states)
            self.assertEqual(len(messages), 1)

    def test_challenge_without_manual_action_times_out(self):
        with LocalFixture("challenge") as fixture, tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "account.json"
            messages = []
            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), report=messages.append, timeout=5)
            with self.assertRaisesRegex(LoginError, "超时"):
                LoginFlow(fixture.provider, fixture.transport, browser, report=lambda _: None).run(fixture.credentials, output)
            self.assertEqual(fixture.steps, ["authorize", "email", "password"])
            self.assertNotIn("/token", fixture.proxy_paths)
            self.assertFalse(output.exists())
            self.assertTrue(messages)

    def test_proxy_interruption_stops_before_next_credential(self):
        with LocalFixture("challenge") as fixture, tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "account.json"

            def stop_proxy(page, state):
                if state == "challenge":
                    fixture.servers[-1].shutdown()
                    fixture.servers[-1].server_close()

            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), manual_handler=stop_proxy, report=lambda _: None, timeout=15)
            with self.assertRaisesRegex(LoginError, "不会回退直连"):
                LoginFlow(fixture.provider, fixture.transport, browser, report=lambda _: None).run(fixture.credentials, output)
            self.assertEqual(fixture.steps, ["authorize", "email", "password"])
            self.assertNotIn("/token", fixture.proxy_paths)
            self.assertFalse(output.exists())

    def test_wrong_callback_state_never_exchanges_code(self):
        with LocalFixture("wrong_state") as fixture, tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "account.json"
            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), report=lambda _: None, timeout=30)
            with self.assertRaisesRegex(LoginError, "state"):
                LoginFlow(fixture.provider, fixture.transport, browser, report=lambda _: None).run(fixture.credentials, output)
            self.assertEqual(fixture.steps.count("password"), 1)
            self.assertNotIn("/token", fixture.proxy_paths)
            self.assertFalse(output.exists())

    def test_unexpected_origin_is_blocked_before_request(self):
        with LocalFixture("unexpected_origin") as fixture, tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "account.json"
            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), report=lambda _: None, timeout=15)
            with self.assertRaises(LoginError):
                LoginFlow(fixture.provider, fixture.transport, browser, report=lambda _: None).run(fixture.credentials, output)
            self.assertEqual(fixture.local_side_effects, 0)
            self.assertFalse(output.exists())

    def test_only_exact_callback_can_bypass_proxy(self):
        with LocalFixture("local_resource") as fixture, tempfile.TemporaryDirectory() as directory:
            browser = ChromeLogin(fixture=True, headless=True, pacer=no_wait(), report=lambda _: None, timeout=30)
            LoginFlow(fixture.provider, fixture.transport, browser, report=lambda _: None).run(fixture.credentials, Path(directory) / "account.json")
            fixture.assert_complete()
            self.assertEqual(fixture.local_side_effects, 0)


if __name__ == "__main__":
    unittest.main()
