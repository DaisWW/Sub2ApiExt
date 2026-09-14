# Sub2API Priority Sync

`priority-sync` 是独立的账号路由优先级策略服务。它直接读取 Sub2API PostgreSQL 的 `accounts`、`usage_logs` 和 `ops_error_logs`，不读取 `monitoring` 的表，也不向 PostgreSQL 写业务数据。

服务默认自动写回，每 10 分钟生成 `/data/priority-report.json`，并通过 `PUT /api/v1/admin/accounts/{id}` 更新账户优先级，不直接更新 `accounts.priority`。将 `PRIORITY_SYNC_DRY_RUN` 设为 `true` 可只生成报告，不调用 Admin API，也不推进本地确认或探索状态。

## 优先级策略

评估权重固定为：

| 维度 | 权重 |
| --- | ---: |
| 综合每百万 Tokens 成本 | 100% |
| 速度 | 0% |
| 可用性 | 0% |
| 失败和 429 | 0% |

服务按账户的真实综合成本做全局排名。成本更低的账户不会得到更大的优先级数值；相同成本得到相同分数，账户数量超过可用整数档位时允许相邻成本并列。有效成本账户按排名映射到 `10..90`，其中数值越小越优先。启动日志和每轮完成日志都会输出：

```text
evaluation_weights="成本=100%,速度=0%,可用性=0%,失败/429=0%"
```

正式成本证据只使用两个滚动窗口：最近 30 分钟达到 20 个计费请求且计费 Tokens 达到 100 万时使用 30 分钟；否则最近 2 小时达到 5 个计费请求且计费 Tokens 达到 50 万时使用 2 小时。两者均不足时保持当前优先级。24 小时和 7 天数据只用于观察，即使成本有效也不会参与正式排名。

```text
综合每百万 Tokens 成本 =
  Σ(有正成本请求的账户真实成本)
  ÷ Σ(这些请求的 input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens)
  × 1,000,000
```

账户真实成本优先取 `account_stats_cost` 或 `total_cost`，乘以请求发生时记录的 `account_rate_multiplier`；这些字段都没有有效值时才回退到 `actual_cost`。缓存读写的 Tokens 和成本已经包含在上式中，因此缓存命中造成的实际成本变化会自然反映到下一轮排名，不再单独加权或冻结。

缺失成本或零成本的日志不会计入计费请求数、计费 Tokens 或成本分母，避免不完整日志制造虚假的低成本。速度、可用性、失败、429、缓存命中率和样本量继续写入 JSON 报告和日志表，仅用于观察。它们不会改变成本分数、目标优先级或阻止成本驱动的提升。

以下执行规则保留：

- 非 `active` 账户，或仍处于 rate limit、临时不可调度、过载冷却窗口的账户，立即设为 `1000`。
- 没有有效成本证据的账户保持当前正式优先级；倍率只触发下述受控恢复试跑，不直接确定正式优先级。
- 普通调整需连续两个周期方向一致；每次最多移动 20 点；实际变更后冷却 15 分钟。
- `PRIORITY_SYNC_EXPLORATION_ENABLED` 默认关闭。显式开启时，仍可暂时把一个无足够样本的账户放到探索档位 20 以收集成本证据。

倍率只作为异常账户的恢复锚点，不进入正式评分。对于没有有效 30 分钟/2 小时成本的账户，服务使用至少两个同平台有效账户计算：

```text
恢复锚点成本 = median(同平台账户近期成本 / 同平台账户倍率) × 当前账户倍率
```

若锚点推导出的成本优先级优于当前值，服务会复用单一探索租约，把一个候选账户放到优先级 20 试跑 10 分钟。试跑结束先立即恢复原优先级，再让新鲜真实成本按正常的两轮确认和步进规则生效。试跑仍贵或出现新的终态失败时按 `2h -> 6h -> 24h` 退避；没有结果时等待 2 小时。Admin API 写入失败不会记作账户试跑失败。账户短暂离开指标快照时会保留租约并按保存的 ID 恢复；确认账户已删除时释放租约，临时写入失败则继续重试。该恢复机制独立于普通探索开关，因此普通探索关闭时，低倍率异常账户仍有受控恢复机会。

排名是账户级全局排名，不再按分组、模型、端点或平台分别归一化。网关仍只会在实际可承接同一请求的候选账户之间使用 `accounts.priority`，且 `account_groups.priority` 的分组档位仍优先于账户优先级；本服务不能跨分组档位改变路由。

## 数据与报告

`usage_logs` 会按请求 ID 去重，避免重试或重复日志重复计入成本。报告中的 `cost_per_million_tokens` 和 `observed_cost_per_million_tokens` 均表示用于排名的直接综合成本；`scoring_window` 只会是实际采用的 `30m` 或 `2h`。`cost_windows` 同时列出 30 分钟、2 小时、24 小时和 7 天的成本、计费请求数、计费 Tokens 及是否满足正式门槛，便于观察但不会让 24 小时/7 天越权参与排名。

每轮日志会输出账户摘要表，账户列显示为 `账户名 #ID`，并包含成本分数、目标优先级、本轮步进值、当前优先级、样本、可用性、综合成本/M、缓存命中率、延迟 P90 和写回状态。无成本账户显示 `-`。

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PRIORITY_SYNC_INTERVAL` | `10m` | 重算周期，最小 30 秒 |
| `PRIORITY_SYNC_WINDOW` | `24h` | 仅保留旧配置兼容；内置策略固定读取 30m、2h、24h 和 7d，不使用此值改变正式窗口 |
| `PRIORITY_SYNC_CHANGE_COOLDOWN` | `15m` | 同一账号两次实际变更的最短间隔 |
| `PRIORITY_SYNC_MIN_SAMPLES` | `5` | 报告置信度和可选探索使用；不限制有效成本参与排名 |
| `PRIORITY_SYNC_CONFIRMATIONS` | `2` | 连续相同调整方向的确认周期数 |
| `PRIORITY_SYNC_EXPLORATION_ENABLED` | `false` | 是否开启无成本账户的有限探索 |
| `PRIORITY_SYNC_DRY_RUN` | `false` | `true` 时只报告，不通过 Admin API 写回 |
| `PRIORITY_SYNC_SUB2API_URL` | `http://sub2api:8080` | Admin API 地址 |

数据库连接使用 `PRIORITY_SYNC_DATABASE_URL`，或标准 `DATABASE_HOST/PORT/USER/PASSWORD/DBNAME/SSLMODE` 变量。部署脚本会设置 `PGOPTIONS=-c default_transaction_read_only=on`，并从 `settings` 表动态读取 Admin API Key；密钥不会写入报告或日志。

## 部署

```bat
priority-sync\deploy.bat
```

运行文件位于 `C:\ProgramData\Sub2API\extensions\priority-sync`，报告和确认/探索状态保存在 Docker 数据卷中。部署后可用 `docker logs sub2api-priority-sync` 查看账户表和评估权重。
