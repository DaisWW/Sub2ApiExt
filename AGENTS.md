# Sub2ApiExt 项目规则

Scope: Sub2ApiExt repository root。

## 常驻规则

- 默认用中文；改动保持最小，保留无关工作区改动。
- 不主动提交 Git；提交时使用固定格式“【模块】动词开头的简单说明”。
- 先读本文件；按任务最多再读两份相关文档，不扫描依赖、运行数据或历史目录。

## 项目边界

- rate-sync/ 和 monitoring/ 是独立 Go 模块，各自维护 go.mod 和测试。
- rate-sync 读取 Sub2API PostgreSQL，倍率只能经 Admin API 写回；不改上游源码或表结构。
- monitoring 读取 PostgreSQL、探测账号并提供只读面板；不得回写网关状态，也不为分组重复探测。
- PowerShell/BAT/Docker 部署把真实配置和状态放在 ProgramData，不依赖 Git 工作区。

## 任务路由

- Bug、崩溃、部署症状：读 .codex/workflows.md 的 Triage Issue。
- 逻辑审查、CR、精简或代码质量：读 .codex/docs/code-review.md。
- 模糊方案：读 Grill Me；写规则/README：读 .codex/docs/doc-writing.md。
- 服务行为看对应 README；部署变更同时看目标脚本和 deploy-common.ps1。

## 编码与安全

- Go 使用 gofmt；IO、SQL、HTTP 和循环传递 context.Context，关闭 Rows/Body/连接，错误保留 %w。
- SQL 参数化；事务、锁、租约、水位、重试和状态更新必须原子、幂等、可恢复。
- goroutine、channel、ticker、worker lease 和数据库连接必须有停止/释放路径；重试有上限。
- 令牌、Admin API Key、数据库密码和代理认证不进入日志、错误、状态文件或提交。
- 监控遵守代理、私网和 SSRF 校验；不得静默绕过配置的代理或只读边界。
- 部署检查外部命令退出码、健康检查和 UAC；不得覆盖 ProgramData 配置、状态卷或密钥。

## 业务不变量

- rate-sync 区分账号/分组模式、稳定 ID、水位、倍率因子和待发布目标；失败不猜测、不重复消费。
- rate-sync 账户配置只使用 `upstream_factors`：键同时是自动同步白名单，值是上游倍率折扣系数而非最终账户倍率（例如 `0.115 × 0.9 = 0.1035`）；禁止直接把 `0.9` 写成账户最终倍率。
- monitoring 区分历史证据、主动探测、分组聚合和告警；单次失败不能抹掉有效历史。

## 完成标准

- 运行受影响模块的最窄测试、格式化或脚本解析检查；无法运行时说明原因和风险。
- 检查完整 diff、调用点、并发/事务、凭据脱敏、兼容性和资源释放。

## Git 提交

- 一条提交只表达一个主题；标题用固定格式“【模块】动词开头的简单说明”，不使用 feat:/fix:。
- 正文写改动与验证；显式列出文件，禁止 git add -A、git commit -a 或宽泛路径。
- 提交后核对 hash、文件数、增删行数和验证结果。
