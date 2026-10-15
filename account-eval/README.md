# 账户测评

适用于管理员保存自定义问题、按平台/分组批量测试账户和对比完整回答。它是独立 Go 服务，不修改 Sub2API 源码、数据库结构、账户状态或渠道监控逻辑；题库、任务和结果存入自己的 SQLite。

## 使用

1. 在独立“题库”中编写并保存题组。每题有名称、问题原文和 HTML/文本/JSON 展示格式，支持排序、复制和删除。“保存并用于测试”直接返回工作台。
2. 在“测试工作台”参照主站账户页，按平台、类型（OAuth / Setup Token / API Key / Bedrock）、状态、隐私模式、分组及名称/ID 搜索组合筛选，再全选当前范围或手动勾选。正常、限流、临时不可调度、停用、错误及已停调采用主站相同的调度开关和到期时间判断。切换筛选保留已选账户，并提示筛选外的选中数量；可一键重置筛选或清空选择。多分组中的同一账户只请求一次。
3. 在“测试题库”下拉选择已保存的题组，默认选中第一个；测试任务自动使用所选题组名称。选择各平台模型、OpenAI 推理强度和账户并发，再“开始测试”。模型下拉合并所选账户关联渠道的模型映射、渠道/分组定价模型及账户模型映射和已保存的模型能力目录，排除通配符规则；支持“刷新模型”和自定义模型。每次打开默认按模型名称的数字版本选择渠道最新文本模型，排除图像、音频等专用模型；无文本渠道模型时回落账户目录，无文本目录时沿用已保存的模型。当前页面手动选择会保留，刷新页面后重新选择最新默认。请求协议、超时和输出 Token 上限放在折叠的“高级参数”中。
4. 工作台下方自动显示当前测试的等待/执行/完成/失败数量、进度和回答；可以继续提交批次。切到“历史记录”查看以往批次，返回工作台保留当前任务。按同一道题对比账户回答，查看隔离的 HTML 预览或原文，下载 HTML/原文、导出批次 JSON，标记通过、不通过或待复核。

OpenAI 推理下拉每次打开默认选中 `max（最高）`，不提供“模型默认”选项；当页手动选择会保留。主站已保存所选模型的 `upstream_model_metadata` 时，按已知账户能力的交集筛选等级，不支持 `max` 时默认选择最高可用等级；不支持推理或没有共同等级时禁用下拉且不发送推理参数。未提供能力资料时，候选等级的实际支持仍由上游决定。`max` 原值传给 Responses 的 `reasoning.effort` 或 Chat Completions 的 `reasoning_effort`，仅应用于 OpenAI / OpenAI compatible。渠道目录提供候选名称，实际请求仍遵循每个账户自己的模型映射，不修改主站或主动同步模型。

批次按提交顺序逐个执行；同一批次账户并行，同一账户的题目按顺序执行，每道题独立对话。提交时固定账户名称/ID、题组原文和请求配置，之后修改题组不影响历史。凭据在每题开始时读取，既不进前端也不进 SQLite。关闭页面不影响后台执行。

取消会停止剩余请求并保存已经读到的回答。服务重启保留已完成结果，把执行中批次的未完成项标记为中断；尚未开始的排队批次继续执行。“重试失败项”创建新批次，只请求原批次中未完成的单元，保留原始记录。保存结果失败时 Worker 停止，避免继续消费上游用量。

## 支持范围

| 平台/账户 | 请求方式 |
| --- | --- |
| OpenAI OAuth / Codex | ChatGPT Codex Responses，使用现有 access token |
| OpenAI API Key | Responses / Chat Completions；自动模式遵循账户 `extra` 的 Responses 能力/强制协议设置 |
| OpenAI compatible、Grok/XAI API Key | Chat Completions，支持手动切换 Responses |
| Anthropic / Claude OAuth、API Key | Messages |
| Gemini API Key | generateContent |

其他平台/认证方式显示“不支持”并记录失败原因，不替换成另一账户执行。OpenAI PAT/Agent Identity、Gemini OAuth、Antigravity、Bedrock 目前不支持。OAuth 令牌由主 Sub2API 维护；本服务不刷新或回写令牌，过期后需等待主服务刷新再重试。尚未复刻上游的专用 TLS 指纹或特殊认证扩展，真实上游的兼容性需实测。

参考 Sub2API v0.2.16 的账户协议实现，直接向指定账户的上游发送完整问题；不调用会修改账户状态、部分协议只发送 `hi` 的 `/api/v1/admin/accounts/:id/test`。遵循账户代理、模型映射、请求头覆盖和私网校验；代理不可用时停止请求，禁止静默直连及携带凭据重定向。

Responses、Chat Completions、Messages 流必须收到有效完成事件；HTTP 200、单独的 `[DONE]` 或提前断开均不等于成功。对缺少或错误标注 Content-Type 的流同时识别 SSE 内容，保留截断/中断时已收到的文本。单响应读取上限 16MB；并发为 1–32，单题超时为 5–3600 秒。Token 上限 0 表示不额外指定；Anthropic API 必填时使用 16384，Codex OAuth 不支持该上限，必须为 0。

测试会直接产生账户的上游用量，不经过 Sub2API 计费和 `usage_logs`；结果中的 Tokens 只记录上游实际返回的统计，没有返回时为 0。该服务是人工对比工作台，不自动给“智力”打分。

## 管理员入口与图标

测评页通过现有 policy-gateway 的 `/account-eval/` 与主站同源提供，复用 `auth_token` 登录状态、域名、HTTPS；不单独暴露宿主机端口。所有题库、账户、任务、结果及菜单 API 都通过主站 `/api/v1/auth/me` 验证有效管理员身份，服务端拒绝普通用户。网关会传递浏览器 UA 和客户端转发地址，以保持上游会话绑定信息。

部署通过 Admin API 合并 ID 为 `account-eval`、`visibility=admin` 的额外菜单，保留其他菜单，不写数据库设置。重复安装更新同一项并保留已有名称和 SVG。主 Sub2API 需支持管理员额外菜单和设置部分更新（已核对 v0.2.16）。

内置 `web/icon.svg`。入口设置可选择并预览不超过 10KB 的 SVG 文件，服务端解析 XML，仅接受静态图形及常用属性，拒绝脚本、事件、外链、图片和实体声明；点击“安装 / 更新管理员入口”后直接写入菜单的 `icon_svg`，无需图床。

HTML 回答保留原文，预览使用仅允许脚本的 opaque-origin sandbox；禁止同源权限、联网、表单、弹窗及顶层跳转。生成页面不能读取主站登录凭据。外部脚本/图片的作品可能需要下载后自行查看。

## 部署

Windows 新机器只需先安装并启动 Docker Desktop，然后运行根目录 `一键部署.bat`；主栈、网络、数据库、镜像、运行配置和持久化数据卷都由综合流程初始化。独立更新使用 `account-eval\deploy.bat`，要求 ProgramData 主栈与 policy-gateway 已运行。升级主服务的原有确认和备份流程保留。

运行目录：`C:\ProgramData\Sub2API\extensions\account-eval`。真实配置只保存在其中的 `database.runtime.env`、`settings.env` 和 `.env`；题库与结果存入 Compose 持久化卷 `sub2api-account-eval_eval-data`。重复部署保留配置和数据。使用该目录的 `manage.bat status|logs|restart|stop|start` 管理服务。

安装器优先用现有 Admin API Key 注册入口；首次安装没有 Key 时，通过 stdin 使用主容器初始化管理员凭据登录，不写入日志、命令参数或额外文件。若管理员密码已改、需要验证码/2FA 或 Admin API 不兼容，会明确报告“服务已部署，菜单未完成”，此时登录主站并打开 `<主站地址>/account-eval/`，点击入口设置里的安装按钮即可。

入口地址优先使用 Sub2API 的 `frontend_url` / `api_base_url`，否则识别局域网 IP 和网关端口。需要固定域名时在运行 `.env` 设置 `EVAL_PUBLIC_URL=https://example.com/account-eval/` 后重新部署；入口域名应与浏览器登录主站使用的域名一致。不同访问别名可从对应的 `/account-eval/` 页面更新菜单地址。代理必须将该路径传到现有网关。

`settings.env` 默认 `EVAL_ALLOW_PRIVATE_HOSTS=false`；确需测试管理员配置的内网上游时显式改为 `true` 并重启。主站/数据库/代理属于管理员配置的可信服务；页面不能提供任意数据库或代理地址。

## 验证

```powershell
go test ./...
node --check web/app.js
node --test model_selection_test.mjs
# 指向独立测试 PostgreSQL，禁止使用生产连接
$env:EVAL_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:5432/account_eval_test?sslmode=disable'
go test -run TestSourceReadOnlyCatalogAndProxy -v
```

Go 测试覆盖完成/截断事件、管理员权限、菜单合并、SVG 校验、题组快照、账户去重、批次/账户调度、取消、重启保留和凭据不序列化。浏览器验证可选运行 `EVAL_BROWSER_ADDR=127.0.0.1:8091` 下的 `TestWorkbenchBrowserFixture`；它只编入测试，使用模拟数据和响应，不进入正式服务。

真实请求验收单独使用 `TestLiveAccountBatch`，仅在显式设置 `EVAL_LIVE_CONFIG` 时执行，会产生上游用量。该变量指向权限受限的 JSON 配置，包含管理员登录的 `email`、`password`、PostgreSQL 连接变量映射 `database`（`PGHOST` 等）及 `accounts` 列表（每项为 `id`、`model`）。同平台账户使用相同模型。测试通过真实主站管理员验证、测评 HTTP API、批次 Worker 和上游请求，每个账户验证一道逻辑题及一道 SVG HTML 题，要求有效完成事件、非空回答和 HTML/SVG 结果；主数据库连接强制只读，题库与历史写入临时 SQLite。配置不要放入仓库或日志，推荐通过 stdin 写入临时容器的 tmpfs，执行 `go test -count=1 -run '^TestLiveAccountBatch$' -v` 后移除容器。
