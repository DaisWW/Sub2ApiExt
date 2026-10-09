"""可见 Chrome 自动化；状态未知或出现额外验证时人工接管。"""

from __future__ import annotations

import ipaddress
import random
import time
from urllib.parse import urlsplit

from flow import Credentials, LoginError


class StepPacer:
    """页面就绪后添加有界停顿；不用于隐藏自动化标识。"""

    def __init__(self, sleep=time.sleep, uniform=random.uniform):
        self.sleep, self.uniform = sleep, uniform

    def pause(self):
        self.sleep(self.uniform(0.4, 1.2))


class ChromeLogin:
    def __init__(self, *, fixture=False, headless=False, timeout=300, pacer=None, manual_handler=None, report=print):
        if headless and not fixture:
            raise LoginError("真实授权必须使用可见 Chrome。")
        self.fixture, self.headless, self.timeout = fixture, headless, timeout
        self.pacer = pacer or StepPacer()
        self.manual_handler, self.report = manual_handler, report
        self.states = []

    @staticmethod
    def _visible(page, selector, *, editable=False):
        candidates = page.locator(selector)
        visible = []
        for index in range(candidates.count()):
            candidate = candidates.nth(index)
            if candidate.is_visible() and candidate.is_enabled() and (not editable or candidate.is_editable()):
                visible.append(candidate)
        if len(visible) > 1:
            raise LoginError("页面有多个可能的输入或提交入口；停止自动操作。")
        return visible[0] if visible else None

    @staticmethod
    def _origin(url):
        parsed = urlsplit(url)
        return parsed.scheme, parsed.hostname, parsed.port or (443 if parsed.scheme == "https" else 80)

    def _state(self, page):
        # Challenges take precedence over fields; never fill behind a challenge.
        text = page.locator("body").inner_text(timeout=2000).casefold()
        challenge = page.locator('iframe[src*="challenges.cloudflare"], iframe[src*="captcha"], [data-step="challenge"]')
        if any(challenge.nth(index).is_visible() for index in range(challenge.count())) or any(
            marker in text for marker in ("verify you are human", "unusual activity", "验证您是人", "异常登录")
        ):
            return "challenge", None
        if self._visible(page, '[role="alert"], [data-step="error"]'):
            return "login_error", None
        field = self._visible(page, 'input[type="email"], input[name="email"]', editable=True)
        if field:
            return "email", field
        field = self._visible(page, 'input[type="password"]', editable=True)
        if field:
            return "password", field
        field = self._visible(page, 'input[autocomplete="one-time-code"], input[name="totp"], input[name="code"]', editable=True)
        if field and any(marker in text for marker in ("authenticator", "authentication app", "验证器", "身份验证应用")):
            return "totp", field
        if field:
            return "other_verification", None
        if self._visible(page, '[data-step="consent"]'):
            return "consent", None
        return "unknown", None

    def authorize(self, url: str, credentials: Credentials, callback, transport):
        from playwright.sync_api import sync_playwright

        approved_origin = self._origin(url)
        finished = set()
        last_state = None
        self._last_manual = None
        state_since = time.monotonic()
        deadline = time.monotonic() + self.timeout
        unexpected_navigation = False

        def check_origin(page):
            if unexpected_navigation or self._origin(page.url) != approved_origin:
                raise LoginError("浏览器离开已确认的授权来源；停止自动输入。")

        def guard_page(page):
            # CDP pauses every redirect hop; Playwright routing only sees the first URL.
            session = page.context.new_cdp_session(page)
            main_frame = session.send("Page.getFrameTree")["frameTree"]["frame"]["id"]

            def guard_request(event):
                nonlocal unexpected_navigation
                request = event["request"]
                target = urlsplit(request["url"])
                redirect = urlsplit(callback.uri)
                navigation = event.get("resourceType") == "Document" and event.get("frameId") == main_frame
                is_callback = (
                    self._origin(request["url"]) == self._origin(callback.uri) and target.path == redirect.path
                    and request["method"] == "GET" and navigation
                )
                try:
                    address = ipaddress.ip_address(target.hostname)
                    local = address.is_loopback or address.is_unspecified or (
                        isinstance(address, ipaddress.IPv6Address) and address.ipv4_mapped and address.ipv4_mapped.is_loopback
                    )
                except ValueError:
                    local = target.hostname == "localhost" or (target.hostname or "").endswith(".localhost")
                unapproved = navigation and self._origin(request["url"]) != approved_origin and not is_callback
                unexpected_navigation = unexpected_navigation or unapproved
                try:
                    if unapproved or (local and not is_callback):
                        session.send("Fetch.failRequest", {"requestId": event["requestId"], "errorReason": "BlockedByClient"})
                    else:
                        session.send("Fetch.continueRequest", {"requestId": event["requestId"]})
                except Exception:
                    # Event callbacks must not leak Playwright diagnostics during shutdown.
                    unexpected_navigation = True

            session.on("Fetch.requestPaused", guard_request)
            session.send("Fetch.enable", {"patterns": [{"urlPattern": "*", "requestStage": "Request"}]})
        try:
            with sync_playwright() as playwright:
                browser = playwright.chromium.launch(
                    channel="chrome", headless=self.headless,
                    # The request guard permits only the exact callback on these hosts.
                    proxy={"server": transport.proxy_url, "bypass": "127.0.0.1,localhost"},
                    args=[
                        "--incognito", "--disable-quic", "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
                        "--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1, EXCLUDE localhost",
                    ],
                )
                try:
                    context = browser.new_context(service_workers="block")
                    context.set_default_timeout(5000)
                    page = context.new_page()
                    guard_page(page)

                    def reject_popup(extra):
                        nonlocal unexpected_navigation
                        unexpected_navigation = True
                        try:
                            extra.close()
                        except Exception:
                            pass

                    context.on("page", reject_popup)
                    page.goto(url, wait_until="domcontentloaded", timeout=20000)
                    while not callback.done.is_set():
                        if time.monotonic() >= deadline:
                            raise LoginError("授权等待超时；会话已停止，没有自动重试登录。")
                        transport.preflight()
                        check_origin(page)
                        state, field = self._state(page)
                        if state != last_state:
                            self.states.append(state)
                            last_state = state
                            state_since = time.monotonic()
                        if state in {"email", "password", "totp"} and state not in finished:
                            field.wait_for(state="visible")
                            self.pacer.pause()
                            transport.preflight()
                            check_origin(page)
                            if state == "totp":
                                code, remaining = credentials.otp(time.time())
                                if remaining < 5:
                                    time.sleep(remaining + 0.1)
                                    code, _ = credentials.otp(time.time())
                                value = code
                            else:
                                value = credentials.email if state == "email" else credentials.password
                            check_origin(page)
                            field.fill(value)
                            self.pacer.pause()
                            submit = self._visible(page, 'button[type="submit"], input[type="submit"], form button:not([type])')
                            if submit is None:
                                raise LoginError("未找到唯一可确认的提交入口；停止自动操作。")
                            transport.preflight()
                            check_origin(page)
                            submit.click()
                            finished.add(state)
                            continue
                        if self.fixture and state == "consent" and state not in finished:
                            self.pacer.pause()
                            page.locator('[data-step="consent"]').click()
                            finished.add(state)
                            continue
                        if state != "unknown" and state in finished:
                            state = "repeated_step"
                        if state != "unknown" or time.monotonic() - state_since >= 1:
                            if state != getattr(self, "_last_manual", None):
                                self.report("需要人工接管：额外验证、授权确认、未知页面或登录错误；不会自动反复提交。")
                                self._last_manual = state
                                if self.fixture and self.manual_handler:
                                    self.manual_handler(page, state)
                        page.wait_for_timeout(200)
                    if unexpected_navigation:
                        raise LoginError("授权出现未确认来源或新窗口；会话已停止。")
                    callback.result()
                finally:
                    browser.close()
        except LoginError:
            raise
        except Exception:
            # Playwright exceptions may include fill values or URLs, so never echo them.
            raise LoginError("浏览器启动、页面操作或代理连接失败；会话已停止。") from None
