# Sub2API Pulse 监控服务

`monitoring/` 是独立的监控 Worker 和匿名只读 Web 面板，用于查看 Sub2API 账户、分组、延迟、错误、历史记录和 Tokens 用量。

服务从 PostgreSQL 自动发现账户及其真实分组关系，优先分析已有请求；只有记录到渠道错误时，才对支持的账户发送最小化恢复探测，复用一份账户结果供内部诊断，并将监控历史写入自己的 `monitoring_*` 表。分组不会重复发送模型请求：分组公开状态只由该分组最新的最终请求结果和成功请求耗时决定，不把成员账户部分可用性展示给用户。面板展示可调度账户、可调度的错误账户及 `status=active` 的分组；用量统计只纳入启用账户。未分组、停用账户和停用分组不会生成展示对象。可调度的错误账户保留为红色诊断对象，但不进入启用统计或分组候选；只有同时存在渠道错误证据时才进入恢复探测。探测失败不会修改 `accounts.status`、`accounts.schedulable` 或任何网关路由状态。

## 探测策略

监控遵循“真实请求优先、错误触发恢复探测”：

- `usage_logs` 中已完成且 `actual_cost > 0` 的真实请求是正常运行的主要证据。真实请求直接更新账户和所属分组，分组不会再为同一账户重复发送请求。
- 没有真实请求不会触发主动探测，也不会因为成功证据过期而例行复验；面板继续采用最近有效状态，只有渠道错误才进入恢复探测，避免空闲账户消耗上游额度。
- 主动探测只在 `ops_error_logs` 出现新的、归属于该账户的非业务限制上游错误时执行；单独的账户 `status=error` 不会触发上游请求。一次错误先执行一次恢复验证；连续失败按 `15 分钟 → 1 小时 → 6 小时 → 24 小时` 退避，并加入稳定抖动。长期无真实流量时，后续恢复重试最低间隔提升到约 `2 小时`，下班后的空闲账户不会持续消耗上游额度。
- 账户的真实请求成功或主动验证成功都会结束当前失败影响并生成恢复证据/告警；如果没有用户请求，只有错误触发的恢复探测能确认“已恢复”，面板会标明确认来源。分组只根据最终请求显示状态，不生成成员聚合告警。只有旧探测失败而没有新的渠道错误时，面板显示“等待渠道证据”，并明确当前不发送上游请求。
- 账户配置或状态更新会使更早的请求和验证结果失效，但不会因此自动发送探测；必须等新的真实渠道错误。可调度的错误账户保留为红色诊断对象，但不进入启用分组及健康统计；`schedulable=false` 的账户直接过滤，不显示、不诊断。
- 监控只主动探测账户，不对分组重复发请求；一个账户的结果会供它所属的所有启用分组复用。活动分组只有同时存在启用渠道和可调度账户时才可路由，否则面板直接显示“失败：无启用渠道或可调度候选”。停用分组和未分组不展示、不参与分组健康告警。
- 分组的账户优先级、绑定优先级和探测结果只用于后台路由诊断，不改变分组卡片的健康颜色。分组最新最终请求失败显示“错误/不可用”；最新成功请求按总耗时小于 20 秒显示“可用”，达到 20 秒显示“可用但延迟高”。没有真实请求证据时显示“待确认”，不猜测为绿色。
- 中间 failover 错误先按请求 ID 聚合；同一请求的最终 2xx 结果会压过早期错误，只有最终 HTTP 失败才进入分组状态和历史。账户候选检查仍保留在监控数据中供内部排障，但不在分组卡片展示部分可用、异常数量或失败暴露比例。
- 模型依次取 `credentials.monitor_model`、最近成功的 `usage_logs.model`、`credentials.model_mapping` 的稳定目标，最后使用平台默认模型。
- 配置代理但代理不可用时，直接记录代理错误，不静默绕过代理直连。

当前主动探测支持：

- OpenAI API Key、OAuth、setup-token；
- OpenAI Compatible、Codex、Grok、xAI API Key；
- Anthropic API Key、OAuth、setup-token；
- Gemini API Key，以及没有 `project_id` 的 AI Studio OAuth；
- Antigravity API Key 中转账户，自动沿用主服务的 `base_url + /antigravity` 规则。

Bedrock、Service Account、Code Assist project OAuth 等需要专用签名或换票流程的协议不会收到不兼容探测，也不会因此产生假告警；它们仍可通过真实请求历史展示健康状态。账户代理支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H。本机会在请求及重定向前解析并拒绝非公网结果；使用代理时目标域名仍由代理解析，代理必须属于可信网络边界。

## 面板能力

默认端口为 `8090`，面板提供：

- 最近 24 小时健康统计与状态轨迹；
- 分组、账户筛选；
- 分组和账户卡片实时显示当前成本倍率（保留四位小数）；
- 最近 24 小时内的窗口观测通过率、首字/首字节中位数，以及成功样本的最快、中位数、P95 延迟；
- 最近 24 小时的 24 段状态轨迹（分组按真实请求成功/最终失败，账户可使用恢复探测补证据）。面板健康颜色只有三种：绿色表示可用且延迟低，黄色表示仍可用但延迟高（成功请求总耗时达到 20 秒），红色表示错误/不可用；没有证据的空档显示中性的“待确认”。
- 模型、分组的 Tokens 与成本分布分析；每百万 Tokens 成本用于总览、账户/分组卡片及趋势分析；
- 账户、分组用量卡片可切换展示四类 Tokens、原始/实际成本、有效倍率、缓存命中率与每百万 Tokens 实际成本；
- 最近 1 小时、24 小时、当天、昨天、7 天、15 天、30 天用量窗口；
- 模型/分组视图可分别切换 Tokens、成本，均使用环图；小时/天趋势柱图按渠道展示，Tokens/成本使用堆叠柱，每百万 Tokens 成本使用并列柱；
- 后台扫描与错误恢复巡检倒计时，扫描开始后自动切换为运行状态；
- 实时监控卡片显示近 5 分钟活跃用户数和有效请求数；账户卡额外显示直接来自中转站 Redis 的当前并发请求数，分组卡不展示并发，避免共享账户造成归属误导；
- 历史明细、账户连续失败/恢复告警、浏览器通知；分组成员聚合只供后台诊断，不生成用户告警。

浏览器仅允许 `GET` 和 `HEAD`。访问者可以刷新、筛选、查看历史和告警，但不能触发探测、确认告警或修改网关数据。后台扫描、错误恢复探测和告警生成只由 Worker 执行。

面板健康统计固定使用最近 24 小时，统一控制窗口观测通过率、首字中位数、最快、中位数、P95 和 24 段状态轨迹；分组状态徽标及卡片底部的“最新证据”只使用该分组最新真实请求证据，用于反映当前状态。

默认 60 秒扫描周期下，后台只扫描数据库并分析已有真实请求；没有渠道错误时不会产生上游探测请求。错误恢复会在首次触发时验证一次，持续失败逐步退避到约 24 小时一次，长期无流量的后续重试最低约 2 小时。分组不会产生额外的主动请求，成员结果和近 24 小时流量统计在同一轮复用；主动探测消耗仍不计入面板的业务“实际消耗”。

真实请求的首字指标使用 `usage_logs.first_token_ms`；主动探测显示的是 HTTP 首响应字节近似值，历史弹窗会明确区分。分组只把 `ops_error_logs` 中按请求 ID 去重后的最终失败合并进窗口观测，跳过中间 failover 行和业务限制错误；主动探测失败仍是独立账户信号。
探测错误只保留错误分类、HTTP 状态和延迟，不保存供应商响应正文；历史旧记录在读取面板时也会去除响应正文。

为排除失败占位日志，健康历史和用量只统计 `actual_cost > 0` 的已完成记录。Tokens 总量为 `input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens`。

用量接口：

```text
GET /api/v1/monitor/usage-ranking?period=24h&limit=10
```

支持 `1h`、`24h`、`today`、`yesterday`、`7d`、`15d`、`30d`。返回启用账户/分组范围内的请求数、四类 Tokens、原始/实际成本、有效倍率、每百万 Tokens 成本、成本分项、包含渠道明细的分钟/小时/天时间桶，以及账户、模型和分组排行。`input_tokens`/`output_tokens` 已包含图片 Tokens；不要再把 `image_input_tokens`/`image_output_tokens` 子集相加。`1h` 使用分钟时间桶，`24h`、`today` 和 `yesterday` 使用小时时间桶，较长窗口使用天时间桶。`today` 为今天 00:00 到当前时刻，`yesterday` 为前一天 00:00 到今天 00:00。`limit` 是每个排行维度各排序指标的上限；排行数组是 Tokens、成本、每百万 Tokens 成本和缓存上下文等维度前列对象的去重并集，因此数组总数可能大于 `limit`，并附带未展开对象的请求数、Tokens 和实际成本。模型排行按规范化模型名聚合，不再按分组、账号、渠道或倍率拆分；`effective_rate_multiplier` 是该模型按原始成本加权后的实际倍率。账户和分组还附带缓存命中率。

实时活动接口：

```text
GET /api/v1/monitor/activity
```

接口每次请求都直接读取中转站 PostgreSQL 和 Redis，以 `target_key` 区分账户和分组；不会返回用户标识、API Key、IP 或会话明细。面板默认每 10 秒轮询，不依赖 60 秒后台扫描周期。

- `active_users`：最近 5 分钟 `usage_logs` 中有效请求的去重用户数；
- `requests`：最近 5 分钟已完成且 `actual_cost > 0` 的请求行数；
- `current_concurrency`：账户卡读取 Redis 中当前占用的账户请求并发槽位，口径与 Sub2API 管理端的 `current_concurrency` 一致；分组卡不展示该字段。接口为兼容现有调用仍可能返回分组成员账户并发汇总，但 Redis 没有分组级并发键，不能从共享账户并发可靠推导本组并发。

`current_concurrency` 比请求日志更实时，但只表示此刻正在运行的请求。普通 HTTP 聊天窗口空闲时不会占用槽位，因此“打开但没有正在请求的 10 个聊天框”仍无法从现有通用数据可靠统计；这需要客户端或中转站增加统一的会话注册。Redis 暂时不可用时，接口保留近 5 分钟统计并把 `concurrency_available` 设为 `false`，面板显示不可用而不是误报为 0。

`MONITORING_CONCURRENCY_SLOT_TTL` 默认 `30m`，应与 Sub2API 的 `gateway.concurrency_slot_ttl_minutes` 保持一致；默认上游配置无需修改。

## 内网安全

面板不需要账号、密码或 API Token。一键部署默认发布到 `0.0.0.0:18090`，同一局域网可通过 `http://<主机名或主机 IP>:18090` 访问；部署脚本会校验或创建仅允许 `Domain/Private` 配置文件和 `LocalSubnet` 来源的 Windows 防火墙规则，无法安全配置时会停止部署。面板包含账户名、Tokens 和成本信息，只应运行在可信局域网，不应发布到公网；用量接口不返回用户排行、用户名、余额或用户级额度明细。若只需本机访问，可在安装目录 `.env` 中把 `MONITORING_BIND_HOST` 改为 `127.0.0.1` 后重新运行部署脚本。

## 一键部署

先启动现有 Sub2API 和 PostgreSQL。部署全部扩展可在仓库根目录双击：

```bat
一键部署.bat
```

只部署监控可在本目录双击：

```bat
deploy.bat
```

安装器只读取现有 Sub2API 的 Docker 网络、PostgreSQL 和 Redis 连接信息，不会重建主服务、数据库或 Redis。它会构建 `sub2api-ext-monitoring:local`，并把独立 Compose、设置及数据库/Redis 运行环境安装到 `C:\ProgramData\Sub2API\extensions\monitoring`。账户凭据只在当前探测请求的内存中使用，不由监控服务另行持久化。

部署脚本不会修改 Sub2API 的菜单或其他系统设置。“渠道监控”菜单请在 Sub2API 中手动添加。监控部署和根目录的一键部署会自动运行 `fix-monitoring-access.ps1`：它读取 Sub2API PostgreSQL 的 `frontend_url` / `api_base_url`、Docker 实际端口以及监控 `.env` / `settings.env`，合并 iframe 白名单，校验绑定地址和 Windows 防火墙，并在配置改变时重启监控；不会调用 Admin API，也不会回写网关数据库。

需要单独修复时，在仓库本地运行：

```bat
fix-monitoring-access.bat
```

有域名的机器建议使用同为 HTTPS 的站点和监控域名；在 Sub2API 自定义菜单中手动填写：

```powershell
https://monitor.example.com
```

没有域名的机器使用该机可被局域网访问的 IP，在 Sub2API 自定义菜单中手动填写：

```powershell
http://192.168.1.20:18090
```

没有域名的机器建议做 DHCP 保留或设置静态 IP；如果 IP 变更，重新运行 `fix-monitoring-access.bat`，并同步修改 Sub2API 菜单 URL。也可以使用公司内网 DNS/稳定主机名替代 IP。

如果通过别名打开 Sub2API，而该别名没有写在数据库的 `frontend_url` / `api_base_url` 中，请把该来源追加到 `C:\ProgramData\Sub2API\extensions\monitoring\settings.env` 的 `MONITORING_FRAME_ANCESTORS`，然后运行修复脚本；不要使用 `*`。父页面是 HTTPS 时，菜单中的监控地址也必须使用 HTTPS，否则浏览器会阻止混合内容 iframe。

如果存在原工作目录部署的 `sub2api-monitoring-standalone`，安装器会在新容器健康后移除旧容器，避免两个监控 Worker 同时运行。监控历史保存在 PostgreSQL，不随旧容器删除。

部署完成后可在运行目录使用：

```bat
manage.bat status
manage.bat logs
manage.bat restart
manage.bat stop
manage.bat start
```

容器没有指向 Git 工程的绑定挂载，部署后启动和重启不依赖本仓库。

## 配置项

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MONITORING_BIND_HOST` | `0.0.0.0` | 安装目录 `.env` 中的主机绑定地址；默认允许局域网访问 |
| `MONITORING_PORT` | `18090` | 安装目录 `.env` 中的主机端口 |
| `MONITORING_LISTEN_ADDR` | `:8090` | 容器内 HTTP 监听地址 |
| `MONITORING_INTERVAL` | `60s` | 后台数据库扫描周期；仅渠道错误会触发上游请求 |
| `MONITORING_REQUEST_TIMEOUT` | `30s` | 单次请求超时 |
| `MONITORING_PROBE_CONCURRENCY` | `8` | 账户探测并发数 |
| `MONITORING_FAILURE_THRESHOLD` | `2` | 连续失败达到该次数后告警 |
| `MONITORING_RECOVERY_THRESHOLD` | `1` | 连续成功达到该次数后恢复告警 |
| `MONITORING_ALLOW_PRIVATE_HOSTS` | `false` | 是否允许显式配置的内网上游地址 |
| `MONITORING_FRAME_ANCESTORS` | `'self'` | 允许嵌入监控面板的来源，使用空格或逗号分隔的 `http://` / `https://` 来源；不允许 `*` |

支持的 OAuth 账户会读取现有访问令牌，并使用与主网关一致的提供商端点和认证形状。API Key 账户的 `base_url` / `endpoint`、`monitor_model` 和兼容字段 `model` 会被尊重。令牌刷新仍由主网关负责；监控不会回写账户凭据或路由状态。
