# Sub2API Rate Sync

独立的倍率同步服务，不修改 Sub2API 源码或数据库结构。分组 worker 每 60 秒从 Sub2API 的 PostgreSQL 自动发现可用渠道：只有一个可用账号的分组直接继承该账号倍率；多个账号的分组使用成功请求记录维护快慢成本记忆并动态调价。账户 worker 每 900 秒读取刀哥和 Lucen 的上游价格并更新账户倍率。两者都通过 Sub2API Admin API 写回。

## 最简配置

默认行为是更新分组倍率。账号 worker 使用同一套探测模板更新账户倍率：

```json
{
  "sync_target": "group",
  "interval": "60s",
  "history_window": "24h",
  "min_history_cost_usd": 0.01
}
```

`sync_target` 可选 `group`（默认）或 `account`。`config.json` 使用 `group`，`account-config.json` 使用 `account`。特殊上游系数只在账户配置中使用；账户配置还设置了 `sync_hosts` 白名单，只处理 `www.codexapis.com`、`xixiapi.io` 和 `lucen.cc`；白名单之外的账号保持手动倍率。

账户 worker 的周期是 `900s`（15 分钟），`confirmations: 1` 表示单次确认即可写回。Lucen/XixiAPI 的上游倍率在写回前统一乘一次 `0.85`，分组 worker 不会再次乘该系数。

账户模式优先读取上游直接价格（NewAPI 的价格表和最新计费日志）；只有直接价格接口不可用时才读取 `/v1/usage`。`usage_bootstrap: true` 可让当前本地倍率仍为 `1.0` 的占位账号在没有新增请求时，用累计 `actual_cost / cost` 先建立候选倍率；已设置过非 `1.0` 倍率的账号不会被累计值覆盖。

为避免两个机制同时写入，当前所有账户记录的 Sub2API 内置 `upstream_billing_rate_sync_enabled` 已显式设为 `false`；`rate-sync` 是唯一的账户倍率写入方。

每批新增成功请求的实际账号成本比：

```text
q = SUM(COALESCE(account_stats_cost, total_cost) × 当前 account_rate_multiplier)
    ÷ SUM(total_cost)
```

由于账号倍率会被账户 worker 更新，动态状态按账号保存未乘倍率的基础成本权重。账号倍率变化时会直接用当前 `accounts.rate_multiplier` 重算快慢估计，不必等待新的请求；Lucen 的 `0.85` 等折扣仍只体现在账号倍率中，分组校准不会再次乘该系数。

多账号分组每分钟只消费上次水位之后的新请求，并维护两个按标准费用衰减的估计器：快速记忆 `$5`、稳定记忆 `$100`。上涨时当 `F-S` 超过 `0.002` 后逐步转向快速估计，达到 `0.012` 时完全跟随；下降时先容忍 `0.006`，随后最多使用 `70%` 快速估计，避免短时切流造成过度降价。单次发布限制为上涨 `+0.020`、下降 `-0.010`，写入死区为 `max(0.001, 当前倍率×1%)`。

没有新增成功请求且账号倍率未变化时，估计和分组倍率完全冻结，不会因夜间空闲或墙上时间漂移。多账号统计状态首次初始化或状态丢失时，按 `1h`、`6h`、`24h` 顺序选择第一个达到至少 30 次请求且标准费用至少 `$5` 的窗口；初始化后锁定增量记忆，不会因空闲重新切换窗口；三个窗口都不足则保持当前倍率。初始化只保存账号成本占比，并把快慢记忆质量归一化到 `$5/$100`，不会把整段历史消费误当成未来置信度。`history_window` 仅保留给旧数据源的兼容路径。单账号分组仍直接继承账户倍率。

动态状态和每个分组的日志 ID 水位会持久化到状态卷，容器重启后从原水位继续，不会重复消费历史记录。更新 Admin API 失败时保留待发布目标，下一分钟重试；这不会重复更新快慢记忆。

账户 worker 的特殊倍率只需按上游域名配置一次：

```json
{
  "factors": {
    "lucen.cc": 0.85,
    "xixiapi.io": 0.85
  }
}
```

默认情况下，同步器完全使用 Sub2API 中账号的代理绑定：账号配置了有效代理就使用该代理，未配置则直连。同步器每轮从 `accounts.proxy_id` 联表读取 `proxies` 的协议、地址、端口和认证信息，不写死 `7890`、`7897` 等端口。只有确实需要让“未绑定代理的账号”统一走某个代理时，才额外配置全局代理和备用列表：

```json
{
  "proxy_url": "http://host.docker.internal:7897",
  "proxy_fallback_urls": [
    "http://host.docker.internal:7890"
  ]
}
```

`proxy_url` 和 `proxy_fallback_urls` 都是可选的上游取价代理；账号在 Sub2API 中配置的有效代理会优先使用。只有当前代理出现超时、连接拒绝、连接重置或 DNS 等网络错误时，才按备用列表顺序尝试；已收到的 HTTP 401/403/404/429/5xx 不会切换代理重放。代理只影响同步器读取上游价格，不会改变网关实际转发路径。当前实现支持中转站代理表里的 `http`/`https` 代理；若以后要使用 `socks5`，需要单独增加 SOCKS5 transport 支持，不能把 SOCKS 端口当 HTTP 代理填写。

规则如下：

- 未配置域名的本地系数默认为 `1.0`。
- 账户 worker 同一域名下新增的可用渠道会自动继承该域名系数，因此 Lucen 只需配置一次 `0.85`。
- 渠道、账号或分组改名不影响同步；内部使用账号 ID 和分组 ID 作为稳定身份。
- 账户 worker 的上游域名变化且仍需非 `1.0` 系数时，才需要修改账户配置中的 `factors`。

以下通用项均可省略，代码默认值分别是 `300s`、`2` 和 `false`；生产配置已显式设置分组 `60s`、账户 `900s` 和单次确认：

```json
{
  "interval": "300s",
  "confirmations": 2,
  "dry_run": false
}
```

## 自动发现范围

每个周期会选择同时满足以下条件的绑定：

- API Key 账号为 `active` 且 `schedulable=true`；
- 分组为 `active`；
- 分组已挂到一个 `active` 渠道；
- 账号有有效的 `base_url` 和 `api_key`。

当前用户侧“可用渠道”页面对应的可用绑定会自动发现。分组 worker 处理历史成本；账户 worker 只处理白名单中的刀哥和 Lucen。itai 等未列入白名单的账号保持手动。

新增渠道满足这些条件后，最长一个同步周期会自动纳入，无需修改 JSON 或重新部署。单账号分组直接跟随唯一账号的已保存倍率，不读取历史用量；账号倍率尚未有效同步时保持当前分组倍率，等待账户 worker 同步，不会回退到上游探测。多账号分组才使用动态快慢记忆，没有足够初始化用量时保持原值等待数据。

## 两套内置价格模板

模板由 Go 代码实现，不再逐渠道配置；仅账户 worker 使用这些模板读取上游价格，分组 worker 不执行上游探测：

1. `sub2api_usage`：读取 `/v1/usage?days=1`，优先使用新增 `actual_cost / cost`；账户模式开启 `usage_bootstrap` 且本地倍率为 `1.0` 时，无新增用量也可使用累计 `actual_cost / cost` 作为保守的初始候选。
2. `newapi_pricing`：每个同步周期都重新读取该 Key 的 `/api/log/token`，从最新一条已计费的真实请求日志确定实际价格组，不缓存价格组或倍率。若 `/api/pricing` 可用，则读取该价格组当前的 `group_ratio`；若价格接口仅允许网页登录，则直接读取最新计费日志中的 `other.group_ratio`。

无法匹配模板时不会猜测，也不会修改倍率。NewAPI 渠道至少需要产生一条已计费的真实请求日志；日志会直接给出本次计费使用的价格组，避免把覆盖模型较多的高价组误判成当前组。每轮若无法读取或解析实时计费日志、日志倍率缺失，或日志中的价格组不在当前价格表中，本地倍率保持不动，并明确记录为同步失败；不会使用旧状态冒充本轮检查成功。分组 worker 不执行上游价格探测；以后遇到第三种稳定价格 API，只需在账户模式代码中新增一个模板适配器，无需扩展每渠道配置。

## 一键部署

先启动现有 Sub2API 和 PostgreSQL。Windows 自带的 PowerShell 5.1 即可；如果已安装 PowerShell 7，脚本会优先使用。

Windows 双击或执行：

```bat
deploy.bat
```

仓库根目录的 `一键部署.bat` 会部署本服务和 monitoring；本目录的 `deploy.bat` 只部署 rate-sync。

部署脚本会自动：

1. 发现 Sub2API 的 Docker 网络和 PostgreSQL 连接信息；
2. 构建 `sub2api-ext-rate-sync:local` 镜像；
3. 将 Compose、配置和数据库连接信息安装到 `C:\ProgramData\Sub2API\extensions\rate-sync`；
4. 更新 `sub2api-rate-sync`（分组）和 `sub2api-rate-sync-account`（账户）。

首次安装会优先迁移现有 rate-sync 容器绑定的两个 JSON 配置；没有旧配置时才从 `*.example.json` 创建。以后重新部署会保留 ProgramData 中的真实配置和 Docker 状态卷。容器挂载来自 ProgramData，部署完成后不依赖 Git 工作区。

运行中的同步容器需要读取 Admin API Key、账号、分组和渠道绑定，因此会连接 PostgreSQL。代码只执行查询，部署时通过 `PGOPTIONS` 强制数据库会话只读；倍率更新始终通过 Admin API 完成。Admin API Key 尚未配置时两个容器都会等待；分组容器每 60 秒、账户容器每 900 秒重新检查，配置后自动开始同步。完整 API Key 不写入配置、状态文件或日志。

容器健康检查会读取状态卷中的 `.health.json`：缺少 Admin API Key 时报告“等待”并保持进程健康，这只表示服务正在运行，不表示倍率已经同步；成功发现但当前没有可同步绑定时视为空闲健康周期；有 Key 且存在绑定后，只有实际检查、稳定、预览或更新成功的周期才会刷新成功时间。连续失败超过约三个同步周期后，健康检查会返回失败。

同步只调用用量查询、价格表和模型列表等 GET 接口，不会发起模型推理，因此正常情况下不消耗 token 或产生模型调用费用；上游仍可能施加普通接口限流。

## 查看日志

双击：

```bat
view-sync-logs.bat
```

只查看最近 200 行、不持续跟踪：

```bat
view-sync-logs.bat once
```

`docker logs sub2api-rate-sync` 查看分组日志，`docker logs sub2api-rate-sync-account` 查看账户日志。每行会在时间戳后增加明显状态标记：`[OK]` 成功或稳定、`[FAIL]` 请求/同步/更新失败、`[SKIP]` 暂不自动或跳过、`[RUN]` 启动/开始周期、`[INFO]` 其他过程信息。标记始终保留，即使日志面板不支持颜色也能快速扫描。

每轮同步完成后还会输出 `[TABLE]` 汇总表。账户 worker 的表格只显示账号倍率，分组 worker 的表格只显示分组倍率；两张表分别按账号或分组去重，每个对象只显示一行。分组表不显示代理，因为分组同步不直接使用代理；账户表中的代理地址会脱敏。分组的动态指标（新增费用、F、S、目标）显示在紧随其后的缩进说明行，避免撑宽主表。使用 `view-sync-logs.bat once` 或对应的 `account-once` 可以查看最近一轮的表格。

如需在支持 ANSI 的终端中给标记着色，可设置 `RATE_SYNC_LOG_COLOR=always`；默认 `auto` 只在交互式终端着色，避免把转义字符写入 Docker 日志采集结果。

`view-sync-logs.bat account` 可持续查看账户 worker，`view-sync-logs.bat account-once` 只查看账户 worker 最近 200 行。

安装后还可以在运行目录使用 `manage.bat status`、`manage.bat logs`、`manage.bat restart`、`manage.bat stop` 和 `manage.bat start`。

## 更新 Sub2API

rate-sync 使用独立目录、镜像、容器和状态卷，正常更新 Sub2API 镜像不会覆盖它。Sub2API 或 PostgreSQL 短暂停机时当前周期会失败，恢复后下个周期继续。

如果未来 Sub2API 修改了账号、分组、渠道绑定表结构，rate-sync 的发现查询需要跟随调整；普通版本更新和渠道增删改名不需要改动。
