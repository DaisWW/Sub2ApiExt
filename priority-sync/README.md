# Sub2API Priority Sync

`priority-sync` 是独立的账号路由优先级策略服务。它直接读取 Sub2API PostgreSQL 的 `accounts`、`usage_logs` 和 `ops_error_logs`，不读取 `monitoring` 的表，也不向 PostgreSQL 写业务数据。

服务默认 `dry-run`，每 5 分钟生成 `/data/priority-report.json`，只在日志中报告建议。确认报告稳定后，把 ProgramData 运行目录的 `settings.env` 中 `PRIORITY_SYNC_DRY_RUN` 改为 `false`；实际更新只调用 `PUT /api/v1/admin/accounts/{id}`，不会直接更新 `accounts.priority`。

## 计算口径

默认统计最近 1 小时，重算周期 5 分钟；连续两个周期得到相同档位后才允许变更，变更后冷却 15 分钟。证据少于 5 次时保持当前优先级。

每个账号的有效成本按账号真实成本计算；统计会保留任一成本字段为正的已完成用量行，即使 `actual_cost` 暂时为零；有账号成本/基础成本时乘以请求发生时的账号倍率，只有这些字段都缺失时才回退到已记录的实际成本：

```text
每百万 Tokens 成本 =
  Σ(CASE WHEN account_stats_cost 或 total_cost 存在
         THEN COALESCE(account_stats_cost, total_cost)
              × COALESCE(account_rate_multiplier, rate_multiplier, 1)
         ELSE actual_cost END)
  ÷ Σ(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens)
  × 1,000,000
```

综合分数为：

```text
Score = 0.60 × 成本分 + 0.25 × 速度分 + 0.15 × 可用性分
```

成本和速度使用账号之间的反向归一化（越低越好）；速度使用成功请求的端到端 `duration_ms` P90，没有该值时退回首 Token P90。可用性按最终成功请求计算：

```text
可用性 = 成功请求数 ÷
  (成功请求数 + 最终失败请求数 + 恢复性 429 的软权重)
```

同一 `request_id` 的账户 429 后最终成功时，不把整次请求算作失败。没有 `Retry-After` 时软权重为 `0.25`；等待时间越长权重越高，达到 30 秒时按一次完整失败计入。额外等待已经由端到端 P90 体现，不重复加罚。当前 `rate_limit_reset_at`、临时不可调度或过载冷却仍在有效期时，账号直接进入不可用档位。

分数映射到有限档位 `1/25/50/75/100`，不可用为 `1000`；数值越小越优先，与 Sub2API 默认优先级 `50` 保持同一尺度。档位避免最高分账号独占全部流量。`accounts.priority` 只能影响账号排序，网关仍先按分组绑定优先级排序，因此它不能单独表达精确的 70/30 流量比例。

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PRIORITY_SYNC_INTERVAL` | `5m` | 重算周期，最小 30 秒 |
| `PRIORITY_SYNC_WINDOW` | `1h` | 统计窗口，范围 15 分钟到 7 天 |
| `PRIORITY_SYNC_CHANGE_COOLDOWN` | `15m` | 同一账号两次实际变更的最短间隔 |
| `PRIORITY_SYNC_MIN_SAMPLES` | `5` | 进入评分所需的最少证据数 |
| `PRIORITY_SYNC_CONFIRMATIONS` | `2` | 连续相同结果的确认周期数 |
| `PRIORITY_SYNC_DRY_RUN` | `true` | `false` 才通过 Admin API 写回 |
| `PRIORITY_SYNC_SUB2API_URL` | `http://sub2api:8080` | Admin API 地址 |

数据库连接使用 `PRIORITY_SYNC_DATABASE_URL`，或标准 `DATABASE_HOST/PORT/USER/PASSWORD/DBNAME/SSLMODE` 变量。部署脚本会设置 `PGOPTIONS=-c default_transaction_read_only=on`，并从 `settings` 表动态读取 Admin API Key；密钥不会写入报告或日志。

## 部署

```bat
priority-sync\deploy.bat
```

综合入口会同时部署该服务。运行文件位于 `C:\ProgramData\Sub2API\extensions\priority-sync`，报告和确认状态保存在 Docker 数据卷中。先查看 `priority-report.json` 和容器日志，再决定是否关闭 `dry-run`。
