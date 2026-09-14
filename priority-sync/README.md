# Sub2API Priority Sync

`priority-sync` 是独立的账号路由优先级策略服务。它直接读取 Sub2API PostgreSQL 的 `accounts`、`usage_logs` 和 `ops_error_logs`，不读取 `monitoring` 的表，也不向 PostgreSQL 写业务数据。

服务默认自动写回，每 10 分钟生成 `/data/priority-report.json` 并通过 `PUT /api/v1/admin/accounts/{id}` 更新账户优先级，不直接更新 `accounts.priority`。将 `PRIORITY_SYNC_DRY_RUN` 设为 `true` 可只报告而不调用 Admin API，也不会推进本地确认/探索状态。

## 计算口径

默认主窗口为最近 24 小时，快速窗口为 6 小时，流量权重窗口为 7 天，重算周期 10 分钟；连续两个周期得到相同调整方向（提升或降级）后才允许变更，变更后冷却 15 分钟。方向一致但目标变化时采用最近一次建议值，方向反转则重新确认。成熟证据优先使用 24 小时最终请求数；24 小时不足 5 次时回退到 6 小时，再回退到 7 天流量窗口，避免把近期无请求但 7 天仍稳定的便宜账户当成冷账户。证据仍不足时向倍率锚点默认值缓慢靠近；关闭自动探索时，低样本账户不会靠倍率锚点自动提升。自动探索默认关闭；只有外部能限制探索流量时才应显式开启。

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

统计先按 `分组 + 分组优先级档位 + 平台 + 请求模型 + 实际上游模型 + 上游端点（若有） + 长上下文标记` 建立比较池。只有当前同组同档位至少有两个可调度账户时，池成本才参与优先级计算；不同候选关系的账户连通分量分别归一化，不会用不可达账户的价格互相压分。优先使用 `usage_logs.requested_model`、`upstream_response_model`，再回退到 `upstream_model` 和 `model`；端点使用 `upstream_endpoint`，缺失则进入未知端点池。每个池保留四类 Tokens 和成本；账户的实测成本按池流量加权，并对缓存结构做标准化。

账户风险成本为：

```text
标准化实测成本 = Σ(池流量占比 × 账户在该池的标准化成本)
模型池观测成本 = 0.7 × 标准化成本(24h) + 0.3 × 标准化成本(6h)
账户级降级口径 = 0.8 × 近期成本 + 0.2 × P75(24h)
低样本成本 = λ × 标准化实测成本 + (1 - λ) × 倍率锚点
λ = Tokens / (Tokens + 20M)

风险成本 = 低样本成本
  + 最终失败率 × max(后备账户未命中成本 P75 - 低样本成本, 0)
  + 0.02 × 恢复性 429 加权比例 × 低样本成本
```

池流量权重为最近 7 天 Tokens 的 90%，账户自身平台内的有效模型池均分剩余 10%，避免小流量模型完全改变全局排序，也避免把其他平台的不可承接流量计入账户成本。后备成本使用同一池其他账户成本的 P75；没有其他账户证据时回退到账户自身成本。

综合分数为：

```text
Score = 0.90 × 风险成本分 + 0.10 × 速度分
```

成本和速度在候选连通分量内使用 P10/P90 截断反向归一化（越低越好）。风险成本差距超过 5% 时，速度不能反转成本顺序；只有成本接近时才使用 90/10 综合分。速度由端到端 `duration_ms` P90（70%）和首 Token P90（30%）组成，缺少一项时把剩余项权重归一到 100%。硬冷却和非 active 账户直接进入不可用档位；最长可用窗口内最后一次成功之后达到 3 次最终失败时立即降级。提升门禁优先使用至少 5 个 24 小时结果；近期已有失败时立即使用近期结果，否则回退到成熟评分窗口。最终成功率低于 95% 时禁止提升，达到 20 个最终结果且低于 90% 时只能保持或降级。候选账户相对同一候选连通分量内成熟账户的全局风险成本中位数优势至少为 8% 才允许提升，缺少同域对照成本时保持当前优先级；成本突变超过 30% 或缓存命中率变化超过 15 个百分点时冻结提升两个周期，冻结期间仍可降级。

最终失败和恢复性 429 仍按请求去重；`ops_error_logs.client_request_id` 优先关联 `usage_logs.request_id`，恢复性 429 不重复增加成功请求证据，等待时间已由端到端延迟体现。明确的上游 429 和 5xx 即使缺少 `error_owner/source/phase` 也会进入失败证据，业务限流和普通 4xx 仍排除。缓存命中率按 `cache_read / (input + cache_creation + cache_read)` 计算；可比池数量、实际评分窗口和尾部失败数会写入 JSON 报告，方便上线观察。

```text
可用性 = 成功请求数 ÷
  (成功请求数 + 最终失败请求数 + 恢复性 429 的软权重)
```

同一关联请求在账户 429 后最终成功时，不把整次请求算作失败。没有 `Retry-After` 时软权重为 `0.25`；等待时间越长权重越高，达到 30 秒时按一次完整失败计入。额外等待已经由端到端 P90 体现，不重复加罚。当前 `rate_limit_reset_at`、临时不可调度或过载冷却仍在有效期时，账号直接进入不可用档位。

## 数据与降级

基础账号查询依赖 `accounts`、`account_groups`、`usage_logs` 和 `ops_error_logs` 的现有核心列（账号归属、请求时间/ID、Token、实际成本、状态和优先级）。`account_groups` 读取失败时本轮停止，因为继续使用跨组成本会产生错误排序。模型维度和分项缓存成本属于增强数据：服务每轮探测 `usage_logs` 列，缺少 `requested_model`、实际上游模型、缓存 Token 或分项成本时，分别回退到 `model`、`unknown`、普通 Token 成本或 `actual_cost`。如果整个数据源都缺少 `usage_logs.group_id`，保留旧的账户级兼容口径；已有分组数据时，不可映射的历史行不参与排序。

当前数据无法还原真实的 failover 次数、后备账户切换链或完整倍率变更历史。后备成本因此使用同一模型池其他账户的未命中成本 P75；没有该证据时回退本池中位成本。要做真实切换成本归因，必须补充带请求关联 ID 的尝试/切换日志，并确保每条 `usage_logs` 保存请求发生时的 `account_rate_multiplier`。

分数映射到细粒度优先级 `10..90`，其中 `20` 保留给探索，不可用为 `1000`；数值越小越优先，中性 50 分保持优先级 50。倍率只在证据不足时作为默认锚点，按倍率分数映射到 `30..90`，低倍率账户仍可得到更靠前且可区分的冷启动位置；有足够请求后完全以请求实测的成本、速度和可用性为准，缓存命中率变化造成的成本上升会触发降级。正常优先级每次最多移动 20 点，异常大跨度仍最多四次收敛。显式开启自动探索后，每次只探索一个 active、未硬排除且证据不足的低优先级账户，最多持续 30 分钟；达到 5 次最终结果后进入正式评分，超时向倍率锚点回退。`accounts.priority` 只能影响同一 `account_groups.priority` 档位内的账号排序，不能跨档位优化，也不能单独表达精确的流量比例。

每轮日志会输出紧凑的账户摘要表，账户列统一显示为 `账户名 #ID`（空名称显示为 `#ID`），并包含分数、最终建议/本轮步骤/当前优先级、样本、可用性、每百万 Tokens 成本、可读的延迟 P90 和中文短状态。dry-run 会展示候选调整和确认状态，但不调用 Admin API；正式上线前应先用 `PRIORITY_SYNC_DRY_RUN=true` 观察至少一个完整缓存周期，再恢复自动写回。

## 配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PRIORITY_SYNC_INTERVAL` | `10m` | 重算周期，最小 30 秒 |
| `PRIORITY_SYNC_WINDOW` | `24h` | 兼容旧数据源的主统计窗口，范围 15 分钟到 7 天；固定窗口数据源同时读取 6h 和 7d |
| `PRIORITY_SYNC_CHANGE_COOLDOWN` | `15m` | 同一账号两次实际变更的最短间隔 |
| `PRIORITY_SYNC_MIN_SAMPLES` | `5` | 进入评分所需的最少证据数 |
| `PRIORITY_SYNC_CONFIRMATIONS` | `2` | 连续相同结果的确认周期数 |
| `PRIORITY_SYNC_EXPLORATION_ENABLED` | `false` | 是否开启无流量预算保证的冷账户探索；只有外部限流时开启 |
| `PRIORITY_SYNC_DRY_RUN` | `false` | `true` 时只报告，不通过 Admin API 写回 |
| `PRIORITY_SYNC_SUB2API_URL` | `http://sub2api:8080` | Admin API 地址 |

数据库连接使用 `PRIORITY_SYNC_DATABASE_URL`，或标准 `DATABASE_HOST/PORT/USER/PASSWORD/DBNAME/SSLMODE` 变量。部署脚本会设置 `PGOPTIONS=-c default_transaction_read_only=on`，并从 `settings` 表动态读取 Admin API Key；密钥不会写入报告或日志。

## 部署

```bat
priority-sync\deploy.bat
```

综合入口会同时部署该服务。运行文件位于 `C:\ProgramData\Sub2API\extensions\priority-sync`，报告和探索/确认状态保存在 Docker 数据卷中。部署后可用 `docker logs sub2api-priority-sync` 查看账户表。
