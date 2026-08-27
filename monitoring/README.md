# Sub2API Pulse 监控服务

`monitoring/` 是独立的监控 Worker 和匿名只读 Web 面板，用于查看 Sub2API 账户、分组、延迟、错误、历史记录和 Tokens 用量。

服务从 PostgreSQL 自动发现账户及其真实分组关系，对支持的账户发送最小化探测请求，复用一份账户结果推断所有所属分组状态，并将监控历史写入自己的 `monitoring_*` 表。分组不会重复发送模型请求：分组状态结合账户优先级、`account_groups.priority` 和近 24 小时真实请求量，模拟网关最可能使用的候选层级。面板和用量统计只展示 `status=active` 且 `schedulable=true` 的账户，以及 `status=active` 的分组；未分组和停用分组不会生成展示对象。处于 `error` 的账户只保留后台恢复探测，不进入启用统计。探测失败不会修改 `accounts.status`、`accounts.schedulable` 或任何网关路由状态。

## 探测策略

监控遵循“历史优先、主动探测兜底”：

- 账户配置或状态最后更新之后，只要有一次成功的真实请求，就直接使用 `usage_logs` 作为健康证据，不再重复发送主动请求。
- 没有有效成功证据的可探测账户立即进行一次主动验证；验证成功后停止主动探测。
- 最近验证失败或账户处于 `error` 状态时，每 15 分钟进行一次恢复巡检；一旦真实请求或主动验证成功，同样停止巡检。
- 账户配置或状态更新会使更早的请求和验证结果失效，从而触发重新验证。错误账户只作为后台恢复对象，不进入启用分组及健康统计。
- 监控只主动探测账户，不对分组重复发请求；一个账户的结果会供它所属的所有启用分组复用。停用分组和未分组不展示、不参与分组健康告警。
- 分组按路由层级判定：账户优先级先于分组绑定优先级；高优先级候选全部不可用但低优先级候选可用时标记为“降级”。高优先级正常、低优先级回退成员失败且近期几乎没有流量时保持“正常”，但在卡片说明中保留成员风险。近 24 小时真实请求量会覆盖无流量时的先验权重，避免低流量失败账户把整个组误报为故障；所有候选均失败才标记为“失败”，仍有未验证候选时不会武断判定全组失败。
- 一次真实请求成功只证明分组当前存在可用路径；如果之后的账户巡检发现分组主候选异常，面板会保留分组级“降级/失败”信号，不让另一条成功历史永久掩盖成员风险，直到新的巡检结果恢复。
- 模型依次取 `credentials.monitor_model`、最近成功的 `usage_logs.model`、`credentials.model_mapping` 的稳定目标，最后使用平台默认模型。
- 配置代理但代理不可用时，直接记录代理错误，不静默绕过代理直连。

当前主动探测支持：

- OpenAI API Key、OAuth、setup-token；
- OpenAI Compatible、Codex、Grok、xAI API Key；
- Anthropic API Key、OAuth、setup-token；
- Gemini API Key，以及没有 `project_id` 的 AI Studio OAuth；
- Antigravity API Key 中转账户，自动沿用主服务的 `base_url + /antigravity` 规则。

Bedrock、Service Account、Code Assist project OAuth 等需要专用签名或换票流程的协议不会收到不兼容探测，也不会因此产生假告警；它们仍可通过真实请求历史展示健康状态。账户代理支持 HTTP、HTTPS、SOCKS5 和 SOCKS5H。直连请求会校验上游解析地址；HTTP(S)/SOCKS 代理由代理端解析目标，因此代理必须属于可信网络边界。

## 面板能力

默认端口为 `8090`，面板提供：

- 24 小时、7 天、30 天健康窗口；
- 分组、账户筛选；
- 所选健康窗口内的可用率、首字/首字节中位数，以及成功样本的最快、中位数、P95 延迟；
- 所选健康窗口的 24 段状态轨迹（真实请求优先、主动探测补空档；无新采样时以较低亮度沿用最近状态），以及失败请求的历史实测响应时间；
- 模型、分组的 Tokens 与成本占比；
- 最近 1 小时、24 小时、当天、7 天、15 天、30 天用量窗口；
- 模型/分组占比环图可分别切换 Tokens/成本；小时/天趋势柱图支持 Tokens/成本切换、纵轴和悬停明细；
- 游戏式探测周期倒计时，探测开始后自动切换为运行状态；
- 历史明细、连续失败/恢复告警、浏览器通知。

浏览器仅允许 `GET` 和 `HEAD`。访问者可以刷新、筛选、查看历史和告警，但不能触发探测、确认告警或修改网关数据。探测周期和告警生成只由后台 Worker 执行。

面板的 `24 小时 / 7 天 / 30 天` 选择会同时控制可用率、首字中位数、最快、中位数、P95 和 24 段状态轨迹；状态徽标及卡片底部的“最近验证”则使用最新有效证据，不受统计窗口限制。

默认 60 秒扫描周期下，没有有效证据的账户只验证到首次成功；持续失败的账户最多每 15 分钟调用一次，即单账户每天最多约 96 次。分组不会产生额外的主动请求，成员结果和近 24 小时流量统计在同一轮复用；主动探测消耗仍不计入面板的业务“实际消耗”。

真实请求的首字指标使用 `usage_logs.first_token_ms`；主动探测显示的是 HTTP 首响应字节近似值，历史弹窗会明确区分。`ops_error_logs` 不合并进可用率，避免重试产生的重复错误行；主动探测失败是独立错误信号。
探测错误只保留错误分类、HTTP 状态和延迟，不保存供应商响应正文；历史旧记录在读取面板时也会去除响应正文。

为排除失败占位日志，健康历史和用量只统计 `actual_cost > 0` 的已完成记录。Tokens 总量为 `input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens`。

用量接口：

```text
GET /api/v1/monitor/usage-ranking?period=24h&limit=10
```

支持 `1h`、`24h`、`today`、`7d`、`15d`、`30d`。返回启用账户/分组范围内的请求数、Tokens 总量、总成本、小时/天时间桶，以及模型和分组排行；排行同时覆盖按 Tokens 与按成本的前列对象。

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

安装器只读取现有 Sub2API 的 Docker 网络和 PostgreSQL 连接信息，不会重建主服务、数据库或 Redis。它会构建 `sub2api-ext-monitoring:local`，并把独立 Compose、设置及数据库运行环境安装到 `C:\ProgramData\Sub2API\extensions\monitoring`。账户凭据只在当前探测请求的内存中使用，不由监控服务另行持久化。

部署脚本不会自动修改 Sub2API 的菜单或其他系统设置；如需入口，请在 Sub2API 的“自定义菜单页面”中手动填写监控地址。

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
| `MONITORING_INTERVAL` | `60s` | 自动探测周期 |
| `MONITORING_REQUEST_TIMEOUT` | `30s` | 单次请求超时 |
| `MONITORING_WINDOW_DAYS` | `7` | 面板默认历史窗口，范围 `1-90` 天 |
| `MONITORING_PROBE_CONCURRENCY` | `8` | 账户探测并发数 |
| `MONITORING_FAILURE_THRESHOLD` | `2` | 连续失败达到该次数后告警 |
| `MONITORING_RECOVERY_THRESHOLD` | `1` | 连续成功达到该次数后恢复告警 |
| `MONITORING_ALLOW_PRIVATE_HOSTS` | `false` | 是否允许显式配置的内网上游地址 |

支持的 OAuth 账户会读取现有访问令牌，并使用与主网关一致的提供商端点和认证形状。API Key 账户的 `base_url` / `endpoint`、`monitor_model` 和兼容字段 `model` 会被尊重。令牌刷新仍由主网关负责；监控不会回写账户凭据或路由状态。
