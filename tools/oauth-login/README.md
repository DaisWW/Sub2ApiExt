# OAuth 登录框架（开发阶段）

适用范围：`tools/oauth-login` 的独立 OAuth/OIDC PKCE 框架。已实现本机模拟演示与真实模式入口；当前验证范围是本机虚构账号，不代表已验证 OpenAI/Codex 实号登录能力。

## 安全边界

- 默认运行本机 fake 演示；演示的 OAuth 服务和模拟代理仅绑定本机，外网请求不会被转发。
- `--live` 才启用真实授权流程。公网 HTTP 请求必须经固定代理 `http://127.0.0.1:7897`；代理不可用时停止，不得直连回退。该代理地址不可由配置覆盖。
- 真实 OAuth 参数尚未确认。示例中的 `client_id`、`authorization_endpoint`、`token_endpoint`、`issuer` 和 `jwks_uri` 故意留空。不得猜测或填入未经确认的客户端参数；真实模式在合法授权入口和参数确认前不可用。
- 真实模式的 `redirect_uri` 必须是明确注册并确认的 HTTP loopback 回调（`127.0.0.1` 或 `localhost`），使用固定非零端口。示例值 `http://127.0.0.1:1455/auth/callback` 仅作字段格式示范，不代表该回调已注册。Demo 使用随机本机端口。
- 只在本地测试使用虚构账号。不要把真实密码、验证码种子、令牌或授权响应提交到仓库。该工具不会自动导入网关。
- 真实模式使用有界面 Chrome 和每次全新的隐私上下文。页面就绪后等待 0.4 至 1.2 秒短随机间隔；不伪造指纹，也不隐藏自动化标识。自动化仍可能被识别，随机等待不保证规避风控。
- 浏览器逐跳检查请求及重定向，只有精确的本机回调可以直接访问；其他 loopback 请求被拦截。公网访问使用代理，并禁用 QUIC、非代理 WebRTC UDP 和公网本地 DNS 解析。
- 验证码挑战、邮箱验证码、页面错误或未知状态会交由操作者接管。交互有总超时；超时或代理异常时失败关闭。
- `output/account.json` 含授权令牌，应按密码保护。Git 忽略真实输入、配置、输出、缓存及虚拟环境。

## 环境与安装

需要 Python 3.10 或更新版本，以及已安装的 Google Chrome。程序使用 `channel="chrome"`，不依赖 Playwright 自带的 Chromium。运行器不会安装依赖或浏览器；TOTP 使用 Python 标准库，无需 `pyotp`。

请在本工具目录创建独立环境，并显式经代理安装固定版本依赖：

```bat
python -m venv .venv
.venv\Scripts\python.exe -m pip install --proxy http://127.0.0.1:7897 -r requirements.txt
```

无需执行 `playwright install`。Chrome 未安装时，程序会停止；依赖安装失败时应检查本机代理。

## 使用

复制模板后，按需填写本地配置和输入；示例账号使用 `example.invalid` 假邮箱、假密码及 RFC 6238 公开测试密钥，不可用于真实登录：

```bat
copy config.example.json config.json
copy input\account.example.json input\account.json
```

默认无参数等同于 `--demo`，只启动本机模拟流程：

```bat
run.bat
run.bat --demo
run.bat --demo --headless
```

`--headless` 仅用于 fake 演示及测试，不能用于真实流程。

真实模式入口如下；默认路径分别为 `config.json`、`input/account.json`、`output/account.json`：

```bat
run.bat --live
run.bat --live --config config.json --input input\account.json --output output\account.json
```

真实模式目前是框架入口。执行前需取得目标服务支持的客户端 ID、授权/令牌端点、issuer、JWKS 地址和注册回调。缺少配置或输入仍是 `.invalid` 示例邮箱时，会在启动浏览器前停止。

## 流程与数据

框架将核心授权模型与浏览器界面分离。流程使用 OAuth `state`、PKCE、OIDC `nonce`，并校验 RS256 签名、issuer、audience、subject、已验证邮箱及过期时间。当前自动识别分步邮箱、密码和身份验证器验证码页面，每步最多自动提交一次。验证码挑战、邮箱验证码、授权确认、错误及未知页面交由人工接管，总超时为 300 秒；跨授权来源导航、新窗口或输入/提交入口有歧义时停止。

输入为单账户 JSON：`email`、`password`、`totp_secret`。输出字段为 `type`、`email`、`account_id`、`access_token`、`refresh_token`、`id_token`、`expires_at`，可交给现有账号流水线标准化模块。三个 Token 完整且校验通过后才原子写入；已有输出若属于其他邮箱则保留原文件。请将真实输出放在默认忽略的 `output/` 中；自定义输入、配置和输出路径需自行纳入 Git 忽略规则。

`expires_at` 从换票请求发起时刻保守计算，并受 Access Token 自身的 `exp` 限制；网络等待与校验耗时不会延长输出的有效期。

`config.example.json` 的 client ID、授权/令牌端点、issuer 和 JWKS 地址均为空字符串，scope 示例为 `openid profile email offline_access`。回调示例仍须由服务方明确注册确认。Demo 使用随机本机端口；真实回调须固定端口。固定代理不属于可配置项。

## 验证

```bat
.venv\Scripts\python.exe -m unittest discover -s tests -v
run.bat --demo --headless
```

测试和 headless 演示只应访问本机 fake 服务。需要验证真实网络路径时，先确认代理进程可用；代理连接失败必须观察到任务停止且没有直连请求。
