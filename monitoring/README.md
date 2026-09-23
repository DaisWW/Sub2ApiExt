# Sub2API Pulse 监控服务

`monitoring/` 是独立的监控 Worker 和匿名只读 Web 面板，用于查看 Sub2API 账户、分组、延迟、错误、历史记录和 Tokens 用量。

服务从 PostgreSQL 自动发现账户及其真实分组关系，优先分析已有请求；只有记录到渠道错误时，才对支持的账户发送最小化恢复探测，复用一份账户结果生成所属分组的聚合检查，并将监控历史写入自己的 `monitoring_*` 表。分组不会重复发送模型请求：分组公开状态只依据成员账户聚合结果，任一账户可用就视为分组可用。实时状态面板展示可调度账户、可调度的错误账户及 `status=active` 的分组；未分组、停用账户和停用分组不会生成实时展示对象。用量统计独立按所选时间窗口内的有效消费记录计算，不因账户或分组后来被停用、设为不可调度或软删除而移除历史消费。可调度的错误账户保留为红色诊断对象；有当前窗口健康证据时仍纳入实时健康统计，但不会进入启用分组候选；只有同时存在渠道错误证据时才进入恢复探测。探测失败不会修改 `accounts.status`、`accounts.schedulable` 或任何网关路由状态。

## 探测策略

监控遵循“真实请求优先、错误触发恢复探测”：

- `usage_logs` 中已完成且 `actual_cost > 0` 的真实请求是正常运行的主要证据。真实请求直接更新账户和所属分组，分组不会再为同一账户重复发送请求。
- 没有真实请求不会触发主动探测，也不会因为成功证据过期而例行复验；面板继续采用最近有效状态，只有渠道错误才进入恢复探测，避免空闲账户消耗上游额度。
- 主动探测只在 `ops_error_logs` 出现新的、归属于该账户的非业务限制上游错误时执行；单独的账户 `status=error` 不会触发上游请求。一次错误先执行一次恢复验证；连续失败按 `15 分钟 → 1 小时 → 6 小时 → 24 小时` 退避，并加入稳定抖动。长期无真实流量时，后续恢复重试最低间隔提升到约 `2 小时`，下班后的空闲账户不会持续消耗上游额度。
- 账户的真实请求成功或主动验证成功都会结束当前失败影响并生成恢复证据/告警；如果没有用户请求，只有错误触发的恢复探测能确认“已恢复”，面板会标明确认来源。分组复用成员账户结果生成聚合状态，不发送重复请求，也不生成成员级聚合告警。只有旧探测失败而没有新的渠道错误时，面板显示“等待渠道证据”，并明确当前不发送上游请求。
- 账户配置或状态更新会使更早的请求和验证结果失效，但不会因此自动发送探测；必须等新的真实渠道错误。可调度的错误账户保留为红色诊断对象，不进入启用分组候选；有当前窗口健康证据时仍纳入健康统计；`schedulable=false` 的账户直接过滤，不显示、不诊断。
- 监控只主动探测账户，不对分组重复发请求；一个账户的健康结果会供它所属的所有启用分组复用。面板把“账户健康”和“分组路由配置”分开显示：活动分组没有启用渠道或可调度账户时标记路由未配置，但不抹掉成员账户的健康证据。停用分组和未分组不展示、不参与分组健康告警。
- 分组的账户优先级和绑定优先级只用于后台路由诊断，不改变“任一账户可用即分组可用”的口径。聚合结果中有低延迟可用账户时显示“可用”；只有可用账户都延迟高时显示“可用但延迟高”；全部已知账户失败且证据充分才显示“错误/不可用”，夹杂待验证账户或没有请求证据时默认按“可用”处理，并标记数据不足。
- 分组状态徽标、通过率和 24 段轨迹使用 `monitoring_checks.source = 'aggregate'` 的账户聚合检查；分组历史明细只展示 `usage_logs.group_id` 严格等于本组的原始请求及其实际经由账户，账户关系不会补造分组请求。
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

- 最近 1 小时健康统计与最近 24 小时状态轨迹；
- 实时监控支持分组、账户及平台下拉筛选，默认选择全部平台；平台选项只来自当前实际存在的对象，Grok/xAI 作为独立平台，不会并入 OpenAI。服务状态卡片可按优先级、名称或首字中位数排序，缺失排序值的对象排在末尾；OpenAI、Anthropic 及其他平台会使用对应筛选颜色。
- 分组和账户卡片实时显示当前成本倍率（保留四位小数）；
- 最近 1 小时内的窗口观测通过率，以及成功样本的首字/首字节最快值、中位数、完整请求总耗时中位数和 P95 延迟；
- 最近 24 小时的 24 段状态轨迹，每段 1 小时（分组使用成员账户聚合检查，账户可使用恢复探测补证据）。面板健康颜色只有三种：绿色表示可用，黄色表示仍可用但阶段性限速或延迟偏高，红色表示有明确失败证据；没有证据的空档默认按可用显示，并标记数据不足。429 尝试率与最终请求成功率分开统计，重试后成功不会被算成用户失败。
- 模型、分组的 Tokens 与成本分布分析；每百万 Tokens 成本用于总览、账户/分组卡片及趋势分析；
- 账户、分组用量卡片默认选择全部平台，并可通过下拉筛选；平台选项只来自当前实际存在的账户或分组，Grok/xAI 单独列出。卡片可切换展示四类 Tokens、原始/实际成本、有效倍率、缓存命中率与每百万 Tokens 实际成本，支持按使用成本、每百万 Tokens 成本、有效倍率或总 Tokens 升序/降序排列，账户视图另支持按账户优先级排列（数值越小越优先），默认按每百万 Tokens 成本升序；筛选只影响账户/分组卡片，KPI、环图和趋势仍按完整时间窗口统计。
- 最近 1 小时、24 小时、当天、昨天、7 天、15 天、30 天用量窗口；
- 模型/分组视图可分别切换 Tokens、成本，均使用环图；小时/天趋势柱图按渠道展示，Tokens/成本使用堆叠柱，每百万 Tokens 成本使用并列柱；
- 后台扫描与错误恢复巡检倒计时，扫描开始后自动切换为运行状态；
- 实时监控卡片显示近 5 分钟活跃用户数和有效请求数；账户卡额外显示直接来自中转站 Redis 的当前并发请求数，分组卡不展示并发，避免共享账户造成归属误导；
- 历史明细、账户连续失败/恢复告警、浏览器通知；分组聚合状态用于卡片展示，不生成成员级用户告警。

浏览器仅允许 `GET` 和 `HEAD`。访问者可以刷新、筛选、查看历史和告警，但不能触发探测、确认告警或修改网关数据。后台扫描、错误恢复探测和告警生成只由 Worker 执行。

面板卡片健康统计固定使用最近 1 小时，控制通过率、首字最快值、首字中位数、完整请求总耗时中位数和 P95；状态轨迹固定使用最近 24 小时并切成 24 段、每段 1 小时。卡片通过率颜色跟随最右侧（当前）轨迹格子的健康状态；分组的通过率、轨迹和状态徽标都使用最新账户聚合检查，账户状态仍按真实请求、渠道错误和恢复探测的证据优先级计算。

默认 60 秒扫描周期下，后台只扫描数据库并分析已有真实请求；没有渠道错误时不会产生上游探测请求。错误恢复会在首次触发时验证一次，持续失败逐步退避到约 24 小时一次，长期无流量的后续重试最低约 2 小时。分组不会产生额外的主动请求，成员结果和近 24 小时流量统计在同一轮复用；主动探测消耗仍不计入面板的业务“实际消耗”。

真实请求的首字指标使用 `usage_logs.first_token_ms`；主动探测显示的是 HTTP 首响应字节近似值，历史弹窗会明确区分。分组卡片状态、通过率和 24 段轨迹只读取账户聚合检查；分组历史明细只读取原始 `usage_logs`，每条真实请求标注实际经由账户，不展示分组聚合行。账户把真实请求、渠道错误和恢复探测合并为时间窗口观测，持久化的真实请求记录仅用于一次性消费水位，不与原始 `usage_logs` 重复计数。
探测错误只保留错误分类、HTTP 状态和延迟，不保存供应商响应正文；历史旧记录在读取面板时也会去除响应正文。

为排除失败占位日志，健康历史和用量只统计 `actual_cost > 0` 的已完成记录。Tokens 总量为 `input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens`。

用量接口：

```text
GET /api/v1/monitor/usage-ranking?period=24h&limit=10
```

支持 `1h`、`24h`、`today`、`yesterday`、`7d`、`15d`、`30d`。返回时间窗口内全部 `actual_cost > 0` 消费记录的请求数、四类 Tokens、原始/实际成本、有效倍率、每百万 Tokens 成本、成本分项、包含渠道明细的分钟/小时/天时间桶，以及账户、模型和分组排行。账户或分组的当前状态只影响实时监控，不影响用量统计；当前资料不存在时使用 ID 回退名称，未分组消费归入“未分组”。`input_tokens`/`output_tokens` 已包含图片 Tokens；不要再把 `image_input_tokens`/`image_output_tokens` 子集相加。`1h` 使用分钟时间桶，`24h`、`today` 和 `yesterday` 使用小时时间桶，较长窗口使用天时间桶。`today` 为今天 00:00 到当前时刻，`yesterday` 为前一天 00:00 到今天 00:00。`limit` 是每个排行维度各排序指标的上限；排行数组是 Tokens、成本、每百万 Tokens 成本、缓存上下文和账户优先级等维度前列对象的去重并集，因此数组总数可能大于 `limit`，并附带未展开对象的请求数、Tokens 和实际成本。账户条目附带当前账户优先级，优先级为空的历史账户不显示该字段；模型排行按规范化模型名聚合，不再按分组、账号、渠道或倍率拆分；`effective_rate_multiplier` 是该模型按原始成本加权后的实际倍率。账户和分组还附带缓存命中率。

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

费用异常分析以已完成的 `usage_logs` 为事实来源，并只读取关联的用户/API Key/账户/渠道显示名称，按用户、API Key、模型和渠道的滚动窗口判断单条请求成本过高、缓存骤降、实际倍率异常、每百万 Tokens 成本异常及可选的个人预算燃烧。另有请求级缓存告警：同一用户/API Key/模型/渠道/分组在当前窗口至少出现 2 条大输入、低命中请求时单独告警，即使窗口平均值尚未异常；如果这些请求对应的同一 `session_id` 在窗口内使用多个账户，邮件会标注“疑似账户切换导致缓存断档”。邮件会把用户的 username/email、API Key 名称和账户名称与原始 ID 一起显示，例如 `刘笑冬 <lxd@example.com> #7`、`Codex #82`、`账户名称 #78`；缺少名称时仍保留 `用户 #7`、`API Key #82` 或 `账户 #78`。每条告警还会列出当前分析窗口内按成本/倍率排序的最多 8 条请求元数据，包括 `usage_logs.id`、UTC 时间、成本、Tokens、缓存、倍率、账户和延迟，便于回查具体记录。它不会读取或发送 session ID、提示词、响应正文、密钥或原始客户端 request_id，也不会修改网关路由、账户状态或额度。请求完成并写入数据库后，默认在一个监控周期内分析；同一异常按冷却时间合并通知。

QQ 邮箱通知使用 SMTP 授权码，不使用 QQ 登录密码。默认连接 `smtp.qq.com:465` 的隐式 TLS；需要 587 端口时将安全模式改为 `starttls`。把以下变量写入 `C:\ProgramData\Sub2API\extensions\monitoring\settings.env`：

```text
MONITORING_COST_EMAIL_USERNAME=your-account@qq.com
MONITORING_COST_EMAIL_PASSWORD=QQ SMTP 授权码
MONITORING_COST_EMAIL_FROM=your-account@qq.com
MONITORING_COST_EMAIL_TO=admin@example.com
```

保存后在该目录运行 `docker compose up -d --force-recreate monitoring`，使新的邮箱变量进入容器。`MONITORING_COST_DAILY_BUDGET=0` 时不启用个人预算燃烧告警；缓存、倍率和单位成本规则仍然工作。日预算按监控容器 `TZ` 的本地自然日计算。未配置完整 SMTP 参数时费用分析不会启动。单位成本和缓存规则需要同一用户/API Key、模型、渠道的历史基线和足够样本；倍率规则没有历史基线时按绝对倍率判断。邮件通知失败不会影响健康探测和面板服务，并会按冷却时间再次尝试；不会记录 SMTP 密码。

费用告警优先使用 `usage_logs.user_id + api_key_id` 作为范围；没有 API Key ID 时退化为用户范围。它不会把同一个用户不同 API Key 的成本混在一起，但仍不能在同一个 Key 内区分多个没有任务标识的进程。

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
| `MONITORING_COST_ALERTS_ENABLED` | `true` | 是否启用费用异常分析 |
| `MONITORING_COST_WINDOW` | `15m` | 当前费用分析窗口 |
| `MONITORING_COST_BASELINE` | `168h` | 历史基线窗口；必须大于当前窗口 |
| `MONITORING_COST_COOLDOWN` | `30m` | 同一异常重复邮件的最短间隔 |
| `MONITORING_COST_MIN_REQUESTS` | `3` | 缓存/单位成本窗口的最少请求数；倍率异常仍可由单条请求触发 |
| `MONITORING_COST_MIN_TOKENS` | `100000` | 费用异常最少 Tokens 数 |
| `MONITORING_COST_CACHE_MISS_MIN_REQUESTS` | `2` | 请求级缓存告警所需的低命中请求数 |
| `MONITORING_COST_CACHE_MISS_INPUT_TOKENS` | `100000` | 请求级缓存告警中每条请求的最少输入 Tokens |
| `MONITORING_COST_MIN_COST` | `0.5` | 窗口费用异常最低成本门槛，单位与 `usage_logs.actual_cost` 相同 |
| `MONITORING_COST_SINGLE_REQUEST_COST` | `5` | 单条请求成本上限；0 表示关闭单条请求告警 |
| `MONITORING_COST_MIN_BASE_COST` | `0.1` | 倍率异常的最低原始成本门槛；避免小数值噪声 |
| `MONITORING_COST_CACHE_BASELINE_MIN` | `0.60` | 缓存告警要求的历史最低命中率 |
| `MONITORING_COST_CACHE_CURRENT_MAX` | `0.20` | 缓存告警要求的当前最高命中率 |
| `MONITORING_COST_CACHE_COST_RATIO` | `1.5` | 缓存异常相对历史单位成本倍数 |
| `MONITORING_COST_UNIT_COST_RATIO` | `1.5` | 单位成本异常相对历史倍数 |
| `MONITORING_COST_MULTIPLIER_RATIO` | `2.0` | 实际倍率相对历史基线的倍数；没有历史基线时按绝对倍率判断 |
| `MONITORING_COST_DAILY_BUDGET` | `0` | 每个用户/API Key 的日预算，单位与 `actual_cost` 相同；0 表示关闭预算燃烧告警 |
| `MONITORING_COST_BURN_RATIO` | `1.5` | 近窗口燃烧速度相对日预算速度倍数 |
| `MONITORING_COST_EMAIL_HOST` | `smtp.qq.com` | SMTP 主机 |
| `MONITORING_COST_EMAIL_PORT` | `465` | SMTP 端口 |
| `MONITORING_COST_EMAIL_SECURITY` | `implicit_tls` | `implicit_tls` 或 `starttls` |
| `MONITORING_COST_EMAIL_USERNAME` | 空 | SMTP 登录邮箱；留空则关闭邮件发送 |
| `MONITORING_COST_EMAIL_PASSWORD` | 空 | SMTP 授权码，不要填写登录密码 |
| `MONITORING_COST_EMAIL_FROM` | 登录邮箱 | 发件地址 |
| `MONITORING_COST_EMAIL_TO` | 空 | 收件地址，多个地址用逗号分隔 |
| `MONITORING_COST_EMAIL_TIMEOUT` | `15s` | 单次 SMTP 连接和发送超时 |

支持的 OAuth 账户会读取现有访问令牌，并使用与主网关一致的提供商端点和认证形状。API Key 账户的 `base_url` / `endpoint`、`monitor_model` 和兼容字段 `model` 会被尊重。令牌刷新仍由主网关负责；监控不会回写账户凭据或路由状态。
