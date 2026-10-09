"""OAuth/OIDC 协议、固定代理传输和兼容 JSON 输出。"""

from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import http.client
import json
import math
import os
import secrets
import socket
import ssl
import tempfile
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlencode, urlsplit


class LoginError(Exception):
    """可安全展示的错误；不包含响应正文、凭据或授权 URL。"""


def read_object(path: Path) -> dict:
    try:
        value = json.loads(path.read_text(encoding="utf-8-sig"))
    except (OSError, UnicodeError, ValueError):
        raise LoginError("无法读取 JSON 文件，请检查文件是否存在及其编码、格式。") from None
    if not isinstance(value, dict):
        raise LoginError("输入必须是一个 JSON 对象。")
    return value


@dataclass(frozen=True)
class Credentials:
    email: str = field(repr=False)
    password: str = field(repr=False)
    totp_secret: str = field(repr=False)

    def __post_init__(self):
        if not isinstance(self.email, str) or self.email.count("@") != 1 or any(c.isspace() for c in self.email):
            raise LoginError("账户邮箱格式无效。")
        if not isinstance(self.password, str) or not self.password:
            raise LoginError("账户密码不能为空。")
        if not isinstance(self.totp_secret, str) or not self.totp_secret.strip():
            raise LoginError("TOTP 密钥不能为空。")
        self._key()

    @classmethod
    def load(cls, path: Path) -> Credentials:
        data = read_object(path)
        if set(data) != {"email", "password", "totp_secret"}:
            raise LoginError("账户 JSON 必须且只能包含 email、password、totp_secret。")
        return cls(**data)

    def _key(self) -> bytes:
        secret = "".join(self.totp_secret.split()).upper()
        try:
            key = base64.b32decode(secret + "=" * (-len(secret) % 8))
        except (binascii.Error, ValueError):
            raise LoginError("TOTP 密钥必须是有效的 Base32 文本。") from None
        if not key:
            raise LoginError("TOTP 密钥不能只包含填充字符。")
        return key

    def otp(self, timestamp: float) -> tuple[str, int]:
        seconds = int(timestamp)
        digest = hmac.digest(self._key(), (seconds // 30).to_bytes(8, "big"), "sha1")
        offset = digest[-1] & 15
        number = int.from_bytes(digest[offset:offset + 4], "big") & 0x7FFFFFFF
        return str(number % 1000000).zfill(6), 30 - seconds % 30


@dataclass(frozen=True)
class Provider:
    client_id: str
    authorization_endpoint: str
    token_endpoint: str
    issuer: str
    jwks_uri: str
    redirect_uri: str
    scope: str = "openid profile email offline_access"
    fixture: bool = False

    @classmethod
    def load(cls, path: Path) -> Provider:
        data = read_object(path)
        allowed = {"client_id", "authorization_endpoint", "token_endpoint", "issuer", "jwks_uri", "redirect_uri", "scope"}
        if set(data) - allowed:
            raise LoginError("配置包含未知字段；真实代理固定为本机 7897，不能通过配置改写。")
        try:
            provider = cls(**data)
        except TypeError:
            raise LoginError("OAuth 配置缺少必需字段。") from None
        provider.validate()
        return provider

    def validate(self):
        values = (self.client_id, self.authorization_endpoint, self.token_endpoint, self.issuer, self.jwks_uri, self.redirect_uri, self.scope)
        if any(not isinstance(value, str) or not value.strip() for value in values):
            raise LoginError("OAuth 参数尚未配置；请先确认客户端、授权端点、issuer、JWKS 和注册回调。")
        for value in (self.authorization_endpoint, self.token_endpoint, self.issuer, self.jwks_uri):
            try:
                url = urlsplit(value)
                port = url.port
            except ValueError:
                raise LoginError("OAuth 端点 URL 无效。") from None
            valid_scheme = url.scheme == "https" or (self.fixture and url.scheme == "http")
            if not valid_scheme or not url.hostname or url.username or url.password or url.fragment or url.query:
                raise LoginError("OAuth 端点必须是无凭据、查询参数和片段的 HTTPS URL。")
            if port is not None and not 1 <= port <= 65535:
                raise LoginError("OAuth 端点端口无效。")
        try:
            callback = urlsplit(self.redirect_uri)
            port = callback.port
        except ValueError:
            raise LoginError("OAuth 回调 URL 无效。") from None
        if (
            callback.scheme != "http" or callback.hostname not in {"127.0.0.1", "localhost"}
            or callback.username or callback.password or callback.query or callback.fragment
            or not callback.path.startswith("/") or port is None or (port == 0 and not self.fixture)
        ):
            raise LoginError("回调必须是已确认的 HTTP loopback URL，真实模式需要固定非零端口。")
        if "openid" not in self.scope.split():
            raise LoginError("此框架需要包含 openid 的 OIDC 授权。")


class ProxyHttpClient:
    """只连接指定本机代理；没有直连或跟随重定向的分支。"""

    LIVE_PROXY = "http://127.0.0.1:7897"

    def __init__(self, proxy_url: str = LIVE_PROXY, fixture_origin: str | None = None):
        parsed = urlsplit(proxy_url)
        if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or not parsed.port:
            raise LoginError("代理必须是本机 HTTP 代理。")
        if fixture_origin is None and proxy_url != self.LIVE_PROXY:
            raise LoginError("真实模式只允许本机 7897 代理。")
        self.host, self.port = parsed.hostname, parsed.port
        self.proxy_url = proxy_url
        self.fixture_origin = fixture_origin

    def preflight(self):
        try:
            with socket.create_connection((self.host, self.port), timeout=3):
                pass
        except OSError:
            raise LoginError("本机代理不可用；已停止，不会回退直连。") from None

    def request_json(self, url: str, form: dict | None = None) -> dict:
        target = urlsplit(url)
        body = urlencode(form).encode("ascii") if form is not None else None
        headers = {"Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        connection = None
        try:
            if target.scheme == "https":
                connection = http.client.HTTPSConnection(self.host, self.port, timeout=15, context=ssl.create_default_context())
                connection.set_tunnel(target.hostname, target.port or 443)
                path = target.path or "/"
                if target.query:
                    path += "?" + target.query
            elif self.fixture_origin and f"{target.scheme}://{target.netloc}" == self.fixture_origin:
                connection = http.client.HTTPConnection(self.host, self.port, timeout=15)
                path = url
            else:
                raise LoginError("拒绝通过非 HTTPS 地址发送授权数据。")
            connection.request("POST" if form is not None else "GET", path, body=body, headers=headers)
            response = connection.getresponse()
            if not 200 <= response.status < 300:
                raise LoginError(f"代理或授权服务返回 HTTP {response.status}；未重试。")
            payload = response.read(1024 * 1024 + 1)
            if len(payload) > 1024 * 1024:
                raise LoginError("授权响应超过大小限制。")
            result = json.loads(payload)
            if not isinstance(result, dict):
                raise LoginError("授权响应不是 JSON 对象。")
            return result
        except (OSError, http.client.HTTPException, ValueError):
            raise LoginError("代理请求或授权响应解析失败；已停止，不会回退直连。") from None
        finally:
            if connection is not None:
                connection.close()


@dataclass(frozen=True, repr=False)
class Authorization:
    state: str = field(default_factory=lambda: secrets.token_urlsafe(32))
    nonce: str = field(default_factory=lambda: secrets.token_urlsafe(32))
    verifier: str = field(default_factory=lambda: secrets.token_urlsafe(32))

    @property
    def challenge(self) -> str:
        return base64.urlsafe_b64encode(hashlib.sha256(self.verifier.encode("ascii")).digest()).decode("ascii").rstrip("=")

    def url(self, provider: Provider, redirect_uri: str) -> str:
        return provider.authorization_endpoint + "?" + urlencode({
            "client_id": provider.client_id, "response_type": "code", "redirect_uri": redirect_uri,
            "scope": provider.scope, "state": self.state, "nonce": self.nonce,
            "code_challenge": self.challenge, "code_challenge_method": "S256",
        })


class CallbackServer:
    def __init__(self, state: str, redirect_uri: str):
        self.state = state
        self.requested = urlsplit(redirect_uri)
        self.done = threading.Event()
        self.code = None
        self.error = None
        self.server = None
        self.thread = None
        self.lock = threading.Lock()

    def __enter__(self):
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                parsed = urlsplit(self.path)
                if self.headers.get("Host") != urlsplit(owner.uri).netloc or parsed.netloc or parsed.path != owner.requested.path:
                    self.send_error(404)
                    return
                query = parse_qs(parsed.query, keep_blank_values=True)
                states = query.get("state", [])
                codes = query.get("code", [])
                valid_state = len(states) == 1 and hmac.compare_digest(states[0].encode("utf-8"), owner.state.encode("utf-8"))
                if not valid_state:
                    error = LoginError("授权回调 state 校验失败。")
                elif "error" in query:
                    error = LoginError("授权服务拒绝了本次授权。")
                elif len(codes) != 1 or not codes[0]:
                    error = LoginError("授权回调缺少唯一有效的授权码。")
                else:
                    error = None
                with owner.lock:
                    if not owner.done.is_set():
                        owner.code, owner.error = (codes[0] if error is None else None), error
                        owner.done.set()
                payload = ("授权回调已接收，可返回控制台。" if error is None else "授权回调未通过校验。").encode("utf-8")
                self.send_response(200 if error is None else 400)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *args):
                pass

        try:
            self.server = ThreadingHTTPServer(("127.0.0.1", self.requested.port), Handler)
        except OSError:
            raise LoginError("本机回调端口不可用；未启动授权。") from None
        self.uri = f"http://{self.requested.hostname}:{self.server.server_port}{self.requested.path}"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        return self

    def result(self) -> str:
        if self.error:
            raise self.error
        if not self.done.is_set() or not self.code:
            raise LoginError("尚未收到有效授权回调。")
        return self.code

    def __exit__(self, *args):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=3)


class TokenValidator:
    @staticmethod
    def _claim(claims: dict, names: set[str]) -> str:
        found = set()
        stack = [claims]
        while stack:
            value = stack.pop()
            if isinstance(value, dict):
                for key, item in value.items():
                    if key in names and isinstance(item, str) and item.strip():
                        found.add(item.strip())
                    if isinstance(item, (dict, list)):
                        stack.append(item)
            elif isinstance(value, list):
                stack.extend(value)
        if len(found) != 1:
            raise LoginError("Token 缺少唯一可识别的账户身份。")
        return found.pop()

    def validate(self, tokens: dict, jwks: dict, provider: Provider, authorization: Authorization, credentials: Credentials, *, requested_at: float) -> dict:
        import jwt

        if any(not isinstance(tokens.get(name), str) or not tokens[name] for name in ("access_token", "refresh_token", "id_token")):
            raise LoginError("授权响应缺少完整的三个 Token；未生成输出。")
        if not isinstance(tokens.get("token_type"), str) or tokens["token_type"].casefold() != "bearer":
            raise LoginError("授权响应不是 Bearer Token。")
        try:
            header = jwt.get_unverified_header(tokens["id_token"])
            if not isinstance(jwks.get("keys"), list) or not all(isinstance(key, dict) for key in jwks["keys"]):
                raise LoginError("JWKS 签名密钥格式无效。")
            keys = [key for key in jwks["keys"] if key.get("kid") == header.get("kid") and key.get("kty") == "RSA"]
            if header.get("alg") != "RS256" or len(keys) != 1:
                raise LoginError("ID Token 签名算法或签名密钥不符合要求。")
            key = jwt.PyJWK.from_dict(keys[0], algorithm="RS256").key
            claims = jwt.decode(
                tokens["id_token"], key, algorithms=["RS256"], audience=provider.client_id, issuer=provider.issuer,
                options={"require": ["iss", "sub", "aud", "exp", "iat", "nonce"]},
            )
        except (jwt.PyJWTError, KeyError, TypeError, ValueError):
            raise LoginError("ID Token 签名、issuer、audience 或有效期校验失败。") from None
        nonce = claims.get("nonce")
        if not isinstance(claims.get("sub"), str) or not claims["sub"].strip():
            raise LoginError("ID Token 缺少有效的 subject。")
        if not isinstance(nonce, str) or not hmac.compare_digest(nonce.encode("utf-8"), authorization.nonce.encode("utf-8")):
            raise LoginError("ID Token nonce 校验失败。")
        audience = claims["aud"]
        if (isinstance(audience, list) and len(audience) > 1 and claims.get("azp") != provider.client_id) or (
            "azp" in claims and claims["azp"] != provider.client_id
        ):
            raise LoginError("ID Token 授权客户端校验失败。")
        email = self._claim(claims, {"email"})
        if email.casefold() != credentials.email.strip().casefold() or claims.get("email_verified") is not True:
            raise LoginError("授权账户与输入邮箱不一致或邮箱未经验证；未生成输出。")
        account_id = self._claim(claims, {"account_id", "chatgpt_account_id"})
        lifetime = tokens.get("expires_in")
        if isinstance(lifetime, bool) or not isinstance(lifetime, (int, float)) or not math.isfinite(lifetime) or lifetime <= 0:
            raise LoginError("授权响应缺少有效的 expires_in。")
        expires_at = int(requested_at + lifetime)
        try:
            access_claims = jwt.decode(tokens["access_token"], options={"verify_signature": False})
            access_exp = access_claims.get("exp")
            if isinstance(access_exp, (int, float)) and not isinstance(access_exp, bool) and math.isfinite(access_exp):
                expires_at = min(expires_at, int(access_exp))
        except jwt.PyJWTError:
            pass
        if expires_at <= time.time():
            raise LoginError("Access Token 已过期；未生成输出。")
        return {"type": "codex", "email": email, "account_id": account_id, **{name: tokens[name] for name in ("access_token", "refresh_token", "id_token")}, "expires_at": expires_at}


class JsonExporter:
    def write(self, record: dict, path: Path):
        if path.exists():
            previous = read_object(path)
            if not isinstance(previous.get("email"), str) or previous["email"].casefold() != record["email"].casefold():
                raise LoginError("输出文件属于其他账户或格式无效；已保留原文件。")
        path.parent.mkdir(parents=True, exist_ok=True)
        temporary = None
        try:
            descriptor, name = tempfile.mkstemp(prefix=".oauth-", suffix=".tmp", dir=path.parent)
            temporary = Path(name)
            with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as stream:
                json.dump(record, stream, ensure_ascii=False, indent=2, allow_nan=False)
                stream.write("\n")
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, path)
        except OSError:
            raise LoginError("JSON 输出失败；未替换原文件。") from None
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)


class LoginFlow:
    def __init__(self, provider: Provider, transport: ProxyHttpClient, browser, report=print):
        self.provider, self.transport, self.browser, self.report = provider, transport, browser, report

    def run(self, credentials: Credentials, output: Path) -> dict:
        self.provider.validate()
        self.transport.preflight()
        self.report("[1/5] 本机代理可连接；启动独立浏览器会话。")
        authorization = Authorization()
        with CallbackServer(authorization.state, self.provider.redirect_uri) as callback:
            self.browser.authorize(authorization.url(self.provider, callback.uri), credentials, callback, self.transport)
            code = callback.result()
            self.report("[2/5] 授权回调与 state 已验证。")
            requested_at = time.time()
            tokens = self.transport.request_json(self.provider.token_endpoint, {
                "grant_type": "authorization_code", "client_id": self.provider.client_id,
                "code": code, "code_verifier": authorization.verifier, "redirect_uri": callback.uri,
            })
        self.report("[3/5] 已通过代理换取授权结果。")
        jwks = self.transport.request_json(self.provider.jwks_uri)
        record = TokenValidator().validate(tokens, jwks, self.provider, authorization, credentials, requested_at=requested_at)
        self.report("[4/5] ID Token 签名、身份和有效期已验证。")
        JsonExporter().write(record, output)
        self.report("[5/5] 已原子写入兼容 JSON。")
        return record
