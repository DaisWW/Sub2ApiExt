"""仅用于本机测试的 OAuth/OIDC 服务与拒绝公网转发的 HTTP 代理。"""

from __future__ import annotations

import base64
import hashlib
import html
import http.client
import json
import secrets
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlencode, urlsplit

from flow import Credentials, LoginError, Provider, ProxyHttpClient


PUBLIC_SECRET = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"


class LocalFixture:
    ORIGIN = "http://oauth.fixture.invalid"

    def __init__(self, scenario="normal"):
        self.scenario = scenario
        self.credentials = Credentials("fixture@example.invalid", "fixture-password-only", PUBLIC_SECRET)
        self.steps = []
        self.proxy_paths = []
        self.blocked_requests = 0
        self.local_side_effects = 0
        self.sessions = {}
        self.codes = {}
        self.lock = threading.Lock()
        self.servers = []
        self.threads = []

    def __enter__(self):
        import jwt
        from cryptography.hazmat.primitives.asymmetric import rsa

        self.private_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
        public = json.loads(jwt.algorithms.RSAAlgorithm.to_jwk(self.private_key.public_key()))
        self.jwks = {"keys": [{**public, "kid": "local-fixture", "use": "sig", "alg": "RS256"}]}
        try:
            upstream = self._start(self._provider_handler())
            self.upstream_port = upstream.server_port
            proxy = self._start(self._proxy_handler())
            self.provider = Provider(
                "local-fixture-client", self.ORIGIN + "/authorize", self.ORIGIN + "/token", self.ORIGIN,
                self.ORIGIN + "/jwks", "http://127.0.0.1:0/auth/callback", fixture=True,
            )
            self.transport = ProxyHttpClient(f"http://127.0.0.1:{proxy.server_port}", fixture_origin=self.ORIGIN)
            return self
        except Exception:
            self.__exit__()
            raise

    def _start(self, handler):
        server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        self.servers.append(server)
        self.threads.append(thread)
        thread.start()
        return server

    def __exit__(self, *args):
        for server in reversed(self.servers):
            server.shutdown()
            server.server_close()
        for thread in self.threads:
            thread.join(timeout=3)

    def issue_tokens(self, nonce, *, email=None, audience=None, lifetime=1800):
        import jwt

        now = int(time.time())
        identity = {
            "iss": self.ORIGIN, "aud": audience or "local-fixture-client", "iat": now, "exp": now + lifetime,
            "sub": "fixture-subject", "nonce": nonce, "email": email or self.credentials.email, "email_verified": True,
            "https://fixture.invalid/auth": {"chatgpt_account_id": "fixture-account-001"},
        }
        access = {"iss": self.ORIGIN, "aud": "fixture-api", "iat": now, "exp": now + lifetime}
        return {
            "token_type": "Bearer", "expires_in": lifetime,
            "access_token": jwt.encode(access, self.private_key, algorithm="RS256", headers={"kid": "local-fixture"}),
            "id_token": jwt.encode(identity, self.private_key, algorithm="RS256", headers={"kid": "local-fixture"}),
            "refresh_token": "fixture-refresh-" + secrets.token_urlsafe(16),
        }

    def assert_complete(self):
        if self.steps != ["authorize", "email", "password", "totp", "consent", "token", "jwks"]:
            raise LoginError("本机模拟未完成全部预期步骤。")
        if "/token" not in self.proxy_paths or "/jwks" not in self.proxy_paths or "/authorize" not in self.proxy_paths:
            raise LoginError("本机模拟没有观察到全部必要的代理请求。")

    def _provider_handler(self):
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def send(self, body, *, status=200, content_type="text/html; charset=utf-8", headers=None):
                payload = body.encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(payload)))
                self.send_header("Cache-Control", "no-store")
                for key, value in (headers or {}).items():
                    self.send_header(key, value)
                self.end_headers()
                self.wfile.write(payload)

            def json(self, value, status=200):
                self.send(json.dumps(value), status=status, content_type="application/json")

            def form(self, action, title, field, input_type="text", extra=""):
                button = '<button>Continue</button>' if owner.scenario == "default_submit" else '<button type="submit">Continue</button>'
                return (
                    '<!doctype html><html><head><meta charset="utf-8"><title>Local OAuth fixture</title></head><body>'
                    f'<h1>{html.escape(title)}</h1><form method="post" action="{action}">'
                    f'<label>{html.escape(title)} <input name="{field}" type="{input_type}" {extra}></label>'
                    f'{button}</form></body></html>'
                )

            def session(self):
                cookies = self.headers.get("Cookie", "").split(";")
                sid = next((item.strip().split("=", 1)[1] for item in cookies if item.strip().startswith("fixture_session=")), "")
                return owner.sessions.get(sid)

            def do_GET(self):
                parsed = urlsplit(self.path)
                if parsed.path == "/authorize":
                    if owner.scenario == "unexpected_origin":
                        self.send("", status=302, headers={"Location": "http://127.0.0.1:" + str(owner.upstream_port) + "/local-side-effect"})
                        return
                    parameters = {key: values[0] for key, values in parse_qs(parsed.query).items() if len(values) == 1}
                    required = {"client_id", "redirect_uri", "state", "nonce", "code_challenge", "code_challenge_method"}
                    if not required <= parameters.keys() or parameters["code_challenge_method"] != "S256":
                        self.json({"error": "invalid_request"}, 400)
                        return
                    sid = secrets.token_urlsafe(16)
                    owner.sessions[sid] = {**parameters, "step": "email"}
                    owner.steps.append("authorize")
                    page = self.form("/email", "Email", "email", "email")
                    if owner.scenario == "local_resource":
                        page += '<iframe src="http://127.0.0.1:' + str(owner.upstream_port) + '/local-side-effect"></iframe>'
                    self.send(page, headers={"Set-Cookie": f"fixture_session={sid}; HttpOnly; SameSite=Lax; Path=/"})
                elif parsed.path == "/local-side-effect":
                    owner.local_side_effects += 1
                    self.send("unexpected local access")
                elif parsed.path == "/jwks":
                    owner.steps.append("jwks")
                    self.json(owner.jwks)
                else:
                    self.send("Not found", status=404)

            def do_POST(self):
                try:
                    length = int(self.headers.get("Content-Length", "0"))
                    if not 0 <= length <= 16384:
                        raise ValueError
                    form = {key: values[0] for key, values in parse_qs(self.rfile.read(length).decode("ascii")).items() if len(values) == 1}
                except (ValueError, UnicodeError):
                    self.json({"error": "invalid_request"}, 400)
                    return
                path = urlsplit(self.path).path
                if path == "/token":
                    with owner.lock:
                        grant = owner.codes.get(form.get("code", ""))
                        verifier = form.get("code_verifier", "")
                        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode("ascii")).digest()).decode("ascii").rstrip("=")
                        valid = (
                            grant and not grant["used"] and form.get("grant_type") == "authorization_code"
                            and form.get("client_id") == grant["client_id"] and form.get("redirect_uri") == grant["redirect_uri"]
                            and challenge == grant["code_challenge"]
                        )
                        if valid:
                            grant["used"] = True
                    if not valid:
                        self.json({"error": "invalid_grant"}, 400)
                        return
                    owner.steps.append("token")
                    self.json(owner.issue_tokens(grant["nonce"]))
                    return
                session = self.session()
                if session is None:
                    self.send('<div role="alert">Session missing</div>', status=400)
                    return
                if path == "/email" and session["step"] == "email" and form.get("email") == owner.credentials.email:
                    owner.steps.append("email")
                    session["step"] = "password"
                    self.send(self.form("/password", "Password", "password", "password"))
                elif path == "/password" and session["step"] == "password" and form.get("password") == owner.credentials.password:
                    owner.steps.append("password")
                    if owner.scenario == "challenge":
                        session["step"] = "challenge"
                        self.send('<h1 data-step="challenge">Verify you are human</h1><form method="post" action="/manual"><button type="submit">模拟人工继续</button></form>')
                    else:
                        session["step"] = "totp"
                        self.send(self.form("/totp", "Authenticator code", "totp", extra='autocomplete="one-time-code"'))
                elif path == "/manual" and session["step"] == "challenge":
                    session["step"] = "totp"
                    self.send(self.form("/totp", "Authenticator code", "totp", extra='autocomplete="one-time-code"'))
                elif path == "/totp" and session["step"] == "totp" and form.get("totp") in {owner.credentials.otp(time.time() + shift)[0] for shift in (-30, 0, 30)}:
                    owner.steps.append("totp")
                    session["step"] = "consent"
                    self.send('<h1>Local authorization consent</h1><form method="post" action="/consent"><button type="submit" data-step="consent">Allow local fixture</button></form>')
                elif path == "/consent" and session["step"] == "consent":
                    owner.steps.append("consent")
                    code = secrets.token_urlsafe(24)
                    owner.codes[code] = {**session, "used": False}
                    state = "wrong-state" if owner.scenario == "wrong_state" else session["state"]
                    location = session["redirect_uri"] + "?" + urlencode({"code": code, "state": state})
                    self.send("", status=302, headers={"Location": location})
                    session["step"] = "complete"
                else:
                    self.send('<div role="alert">Invalid credentials or unexpected step</div>', status=400)

        return Handler

    def _proxy_handler(self):
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_CONNECT(self):
                owner.blocked_requests += 1
                self.send_error(403, "Local fixture does not forward public traffic")

            def forward(self):
                target = urlsplit(self.path)
                if f"{target.scheme}://{target.netloc}" != owner.ORIGIN:
                    owner.blocked_requests += 1
                    self.send_error(403, "Local fixture only")
                    return
                path = target.path or "/"
                owner.proxy_paths.append(path)
                if target.query:
                    path += "?" + target.query
                length = int(self.headers.get("Content-Length", "0"))
                body = self.rfile.read(length) if length else None
                headers = {key: value for key, value in self.headers.items() if key.lower() not in {"host", "proxy-connection", "connection", "transfer-encoding"}}
                connection = http.client.HTTPConnection("127.0.0.1", owner.upstream_port, timeout=5)
                try:
                    connection.request(self.command, path, body, headers)
                    response = connection.getresponse()
                    payload = response.read()
                    self.send_response(response.status)
                    for key, value in response.getheaders():
                        if key.lower() not in {"connection", "transfer-encoding", "content-length"}:
                            self.send_header(key, value)
                    self.send_header("Content-Length", str(len(payload)))
                    self.end_headers()
                    self.wfile.write(payload)
                except (OSError, http.client.HTTPException):
                    self.send_error(502, "Local fixture unavailable")
                finally:
                    connection.close()

            do_GET = forward
            do_POST = forward

        return Handler
