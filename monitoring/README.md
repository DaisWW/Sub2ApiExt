# Sub2API Pulse 监控服务

`monitoring/` 是独立的监控 Worker 和匿名只读 Web 面板，用于查看 Sub2API 账户、分组、延迟、错误、历史记录和 Tokens 用量。

服务从 PostgreSQL 自动发现账户及其真实分组关系，对支持的账户发送最小化探测请求，聚合分组状态，并将监控历史写入自己的 `monitoring_*` 表。面板和用量统计只展示 `status=active` 且 `schedulable=true` 的账户，以及 `status=active` 的分组；未分组和停用分组不会生成展示对象。处于 `error` 的账户只保留后台恢复探测，不进入启用统计。探测失败不会修改 `accounts.status`、`accounts.schedulable` 或任何网关路由状态。

## 探测策略

监控遵循“历史优先、主动探测兜底”：

- 健康账户在一个探测周期内出现近期真实请求时，直接使用 `usage_logs` 作为健康信号，不重复发送主动请求。
- 已处于 `error` 状态的账户仅作为恢复探测对象，即使没有新流量，也能及时发现恢复。
- 没有近期成功历史的可探测账户自动进入主动探测。
- 分组主动探测仅针对状态为“启用”且包含可用成员的分组；停用分组和未分组不展示、不参与主动探测或失败告警。
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
- 可用率、首字/首字节，以及成功样本的最快、中位数、P95 延迟；
- 所选健康窗口的 24 段状态轨迹（真实请求优先、主动探测补空档），以及失败请求的历史实测响应时间；
- 模型、分组的 Tokens 与成本占比；
- 最近 1 小时、24 小时、当天、7 天、15 天、30 天用量窗口；
- 模型/分组占比环图可分别切换 Tokens/成本；小时/天趋势柱图支持 Tokens/成本切换、纵轴和悬停明细；
- 游戏式探测周期倒计时，探测开始后自动切换为运行状态；
- 历史明细、连续失败/恢复告警、浏览器通知。

浏览器仅允许 `GET` 和 `HEAD`。访问者可以刷新、筛选、查看历史和告警，但不能触发探测、确认告警或修改网关数据。探测周期和告警生成只由后台 Worker 执行。

真实请求的首字指标使用 `usage_logs.first_token_ms`；主动探测显示的是 HTTP 首响应字节近似值，历史弹窗会明确区分。`ops_error_logs` 不合并进可用率，避免重试产生的重复错误行；主动探测失败是独立错误信号。
探测错误只保留错误分类、HTTP 状态和延迟，不保存供应商响应正文；历史旧记录在读取面板时也会去除响应正文。

为排除失败占位日志，健康历史和用量只统计 `actual_cost > 0` 的已完成记录。Tokens 总量为 `input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens`。

用量接口：

```text
GET /api/v1/monitor/usage-ranking?period=24h&limit=10
```

支持 `1h`、`24h`、`today`、`7d`、`15d`、`30d`。返回启用账户/分组范围内的请求数、Tokens 总量、总成本、小时/天时间桶，以及模型和分组排行；排行同时覆盖按 Tokens 与按成本的前列对象。

## 内网安全

面板不需要账号、密码或 API Token，安装器默认只发布到 `127.0.0.1:18090`。如需公司局域网访问，可在安装后的 `.env` 中把 `MONITORING_BIND_HOST` 改为 `0.0.0.0`，并通过主机防火墙、反向代理或网络策略限制访问范围。面板包含账户名、Tokens 和成本信息，不应发布到公网；用量接口不返回用户排行、用户名、余额或用户级额度明细。

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
| `MONITORING_BIND_HOST` | `127.0.0.1` | 安装目录 `.env` 中的主机绑定地址 |
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
