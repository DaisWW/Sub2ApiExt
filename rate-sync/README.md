# Sub2API Rate Sync

独立的倍率同步服务，不修改 Sub2API 源码或数据库结构。分组 worker 每 300 秒从 Sub2API 的 PostgreSQL 自动发现可用渠道：只有一个可用账号的分组直接继承该账号倍率；多个账号的分组才用最近历史用量按账号成本加权计算。账户 worker 每 900 秒读取刀哥和 Lucen 的上游价格并更新账户倍率。两者都通过 Sub2API Admin API 写回。

## 最简配置

默认行为是更新分组倍率。账号 worker 使用同一套探测模板更新账户倍率：

```json
{
  "sync_target": "group",
  "interval": "300s",
  "history_window": "24h",
  "min_history_cost_usd": 0.01,
  "factors": {
    "lucen.cc": 0.85,
    "xixiapi.io": 0.85
  }
}
```

`sync_target` 可选 `group`（默认）或 `account`。`config.json` 使用 `group`，`account-config.json` 使用 `account`。账户配置还设置了 `sync_hosts` 白名单，只处理 `www.codexapis.com`、`xixiapi.io` 和 `lucen.cc`；白名单之外的账号保持手动倍率。

账户 worker 的周期是 `900s`（15 分钟），`confirmations: 2` 要求连续两次观察到相同上游倍率才写回。Lucen/XixiAPI 的上游倍率在写回前统一乘一次 `0.85`，分组 worker 不会再次乘该系数。

账户模式优先读取上游直接价格（NewAPI 的价格表和最新计费日志）；只有直接价格接口不可用时才读取 `/v1/usage`。`usage_bootstrap: true` 可让当前本地倍率仍为 `1.0` 的占位账号在没有新增请求时，用累计 `actual_cost / cost` 先建立候选倍率；仍需连续两次确认，已设置过非 `1.0` 倍率的账号不会被累计值覆盖。

为避免两个机制同时写入，当前所有账户记录的 Sub2API 内置 `upstream_billing_rate_sync_enabled` 已显式设为 `false`；`rate-sync` 是唯一的账户倍率写入方。

分组历史成本公式：

```text
分组倍率 = SUM(COALESCE(account_stats_cost, total_cost) × account_rate_multiplier)
           ÷ SUM(total_cost)
```

由于你的账号倍率由管理员手动维护且基本固定，校准优先使用当前 `accounts.rate_multiplier`；只有账号记录没有可用倍率时才回退到日志快照。Lucen 的 `0.85` 等折扣应只体现在账号倍率中，分组校准不会再次乘该系数。

多账号分组每轮一次性统计 `1h -> 6h -> 12h -> 24h`，选择第一个同时满足“请求数不少于 30 且标准费用不少于 5 USD”的窗口。短窗口样本充足时优先使用，样本不足就自动退化到更长窗口；四个窗口都不足时保持当前倍率，并把缺少的请求数或标准费用写入日志和汇总表。`history_window` 是最大窗口上限（默认 `24h`）。单账号分组仍直接继承账户倍率，不受历史窗口影响。绑定了配置了非 1.0 域名系数、但账号倍率仍是 `1.0` 的账号时，为避免误调也保持不动并记录日志。

为避免滚动窗口造成倍率抖动，只有绝对变化达到 `0.005` 或相对变化达到 `1%` 才写回；每轮仍会记录计算结果。

特殊倍率只需按上游域名配置一次：

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
- 同一域名下新增的可用渠道会自动继承该域名系数，因此 Lucen 只需配置一次 `0.85`。
- 渠道、账号或分组改名不影响同步；内部使用账号 ID 和分组 ID 作为稳定身份。
- 上游域名变化且仍需非 `1.0` 系数时，才需要修改 `factors`。

以下通用项均可省略，默认值分别是 `300s`、`2` 和 `false`：

```json
{
  "interval": "300s",
  "confirmations": 2,
  "dry_run": false,
  "factors": {
    "lucen.cc": 0.85
  }
}
```

## 自动发现范围

每个周期会选择同时满足以下条件的绑定：

- API Key 账号为 `active` 且 `schedulable=true`；
- 分组为 `active`；
- 分组已挂到一个 `active` 渠道；
- 账号有有效的 `base_url` 和 `api_key`。

当前用户侧“可用渠道”页面对应的可用绑定会自动发现。分组 worker 处理历史成本；账户 worker 只处理白名单中的刀哥和 Lucen。itai 等未列入白名单的账号保持手动。

新增渠道满足这些条件后，最长一个同步周期会自动纳入，无需修改 JSON 或重新部署。单账号分组直接跟随唯一账号的已保存倍率，不读取历史用量；账号倍率尚未有效同步时才回退到上游探测。多账号分组才使用历史成本加权优化，没有足够历史用量时保持原值等待数据。

## 两套内置价格模板

模板由 Go 代码实现，不再逐渠道配置：

1. `sub2api_usage`：读取 `/v1/usage?days=1`，优先使用新增 `actual_cost / cost`；账户模式开启 `usage_bootstrap` 且本地倍率为 `1.0` 时，无新增用量也可使用累计 `actual_cost / cost` 作为保守的初始候选。
2. `newapi_pricing`：每个同步周期都重新读取该 Key 的 `/api/log/token`，从最新一条已计费的真实请求日志确定实际价格组，不缓存价格组或倍率。若 `/api/pricing` 可用，则读取该价格组当前的 `group_ratio`；若价格接口仅允许网页登录，则直接读取最新计费日志中的 `other.group_ratio`。

无历史用量时的上游探测兜底最终倍率：

```text
round4(上游倍率 × 本地域名系数)
```

无法匹配模板时不会猜测，也不会修改倍率。NewAPI 渠道至少需要产生一条已计费的真实请求日志；日志会直接给出本次计费使用的价格组，避免把覆盖模型较多的高价组误判成当前组。每轮若无法读取或解析实时计费日志、日志倍率缺失，或日志中的价格组不在当前价格表中，本地倍率保持不动，并明确记录为同步失败；不会使用旧状态冒充本轮检查成功。以后遇到第三种稳定价格 API，只需在代码中新增一个模板适配器，无需扩展每渠道配置。

## 一键部署

Windows 自带的 PowerShell 5.1 即可；如果已安装 PowerShell 7，脚本会优先使用。部署前需要先启动官方 Sub2API、PostgreSQL 容器。

Windows 双击或执行：

```bat
deploy.bat
```

部署脚本会自动：

1. 发现 Sub2API 的 Docker 网络和 PostgreSQL 连接信息；
2. 构建 `sub2api-ext-rate-sync:local` 镜像；
3. 将 Compose、配置和数据库连接信息安装到 `C:\ProgramData\Sub2API\extensions\rate-sync`；
4. 启动 `sub2api-rate-sync`（分组）和 `sub2api-rate-sync-account`（账户）。

首次安装会优先迁移现有 `rate-sync` 容器绑定的配置；没有旧容器时才从两个 `*.example.json` 创建。以后重新部署会保留 ProgramData 中已经调整过的配置和状态卷。容器挂载的配置来自 ProgramData，部署完成后不依赖本 Git 工作区。

运行中的同步容器需要读取 Admin API Key、账号、分组和渠道绑定，因此会连接 PostgreSQL。代码只执行查询，部署时通过 `PGOPTIONS` 强制数据库会话只读；倍率更新始终通过 Admin API 完成。Admin API Key 尚未配置时两个容器都会等待；分组容器每 300 秒、账户容器每 900 秒重新检查，配置后自动开始同步。完整 API Key 不写入配置、状态文件或日志。

同步只调用用量查询、价格表和模型列表等 GET 接口，不会发起模型推理，因此正常情况下不消耗 token 或产生模型调用费用；上游仍可能施加普通接口限流。

## 查看日志

`docker logs sub2api-rate-sync` 查看分组日志，`docker logs sub2api-rate-sync-account` 查看账户日志。每行会在时间戳后增加明显状态标记：`[OK]` 成功或稳定、`[FAIL]` 请求/同步/更新失败、`[SKIP]` 暂不自动或跳过、`[RUN]` 启动/开始周期、`[INFO]` 其他过程信息。标记始终保留，即使日志面板不支持颜色也能快速扫描。

每轮同步完成后还会输出 `[TABLE]` 汇总表，字段包括账号、分组、账户倍率、分组倍率、脱敏后的代理地址和本轮结果。账户 worker 与分组 worker 各自输出一张表。

如需在支持 ANSI 的终端中给标记着色，可设置 `RATE_SYNC_LOG_COLOR=always`；默认 `auto` 只在交互式终端着色，避免把转义字符写入 Docker 日志采集结果。

安装后也可以在运行目录执行：

```bat
manage.bat status
manage.bat logs
manage.bat restart
```

## 更新 Sub2API

rate-sync 使用独立目录、镜像、容器和状态卷，正常更新 Sub2API 镜像不会覆盖它。Sub2API 或 PostgreSQL 短暂停机时当前周期会失败，恢复后下个周期继续。

如果未来 Sub2API 修改了账号、分组、渠道绑定表结构，rate-sync 的发现查询需要跟随调整；普通版本更新和渠道增删改名不需要改动。
