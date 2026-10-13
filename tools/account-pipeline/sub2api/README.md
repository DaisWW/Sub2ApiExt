# Sub2API 模块

使用 Python 读取标准化的 `sub2api-accounts.json`，调用 Sub2API Admin API，完成管理员登录、分组和代理校验、账号幂等导入及变更字段显示。总流程中的来源可以是 Redeem 生成的 JSON，也可以是 `input\accounts.txt` 直接账户文本；两者先由 Normalize 统一格式。Cockpit 本地账号读取只保留为备选入口。只允许配置的本机回环地址，错误信息不会输出响应正文或凭据。

账户配置支持统一默认值、目录设置、按邮箱的 `accounts` 和按兑换码的 `redeem_codes`，规则见上级 README。目录设置和兑换码覆盖由总入口关联实际账户；本模块直接读取 JSON 或 Cockpit 账户时应用主配置默认值和邮箱覆盖。生成的 Sub2API 输入在每个账户的 `config` 中保留最终设置，与 `credentials` 分开保存；单独重跑该 JSON 会使用相同的分组和设置。增量中仅配置变化的记录带有 `settings_only: true`，使用账户更新接口并保留服务器已有凭据及未配置的 `extra` 键；单独重跑这些记录仍只更新设置，不调用凭据导入接口，也不会重建不存在的账户。所有账户的设置、分组和代理会在第一个账户写入前完成校验，单账户校验失败只跳过该账户。

账户匹配、导入、后续设置更新或身份复核失败时，记录脱敏原因并继续下一账户。写入前必须匹配输入主标识；输入有邮箱时，不能仅凭相同 account ID 更新不同邮箱的远端账户。成功映射只包含完成更新并复核成功的账户，另用 `failed_accounts` 保存失败标识与原因；有失败项时处理完全部账户后返回退出码 `1`。管理员登录、公共配置结构和账户列表等共享前置步骤失败仍会停止。

默认情况下，没有 `extra.account_pipeline_managed=account-pipeline-v1` 标记的已有账户会被视为手动账户并跳过。需要一次性接管当前输入中精确匹配的已有账户时，在本机配置的 `defaults` 中设置 `claim_existing_accounts` 为 `true`；导入会保留账户 ID，并通过正常更新请求写入归属标记和当前配置。完成认领后建议恢复为 `false`，继续保护以后新增的手动账户。

单独运行：

```bat
run.bat <sub2api-accounts.json> --config <配置文件>
import-from-cockpit-tools.bat [--what-if]
```

`run.bat` 用于导入 JSON；`import-from-cockpit-tools.bat` 才会读取当前 Windows 用户的 Cockpit Tools 加密账号，需要安装 `requirements.txt` 中的 `cryptography` 包。后者不会被总流程自动调用。

总目录的 `incremental.bat` 会使用本模块只读查询带有 `extra.account_pipeline_managed=account-pipeline-v1` 归属标记的账号，用于建立首次增量基线；未匹配账户仍进入逐账户导入和归属检查。输入中减少的账号会把 ID、邮箱和原因写入 `..\cache\results\sub2api-pending-deletions.txt` 供手动处理；本模块不会调用删除接口，也不会按模糊名称或未知 ID 删除。没有归属标记的手动中转账户会跳过，不会被更新或写入工具快照。

总目录的 `refresh-tokens.bat` 复用本模块的凭据专用更新。它逐账号调用 `bulk-update` 的凭据键级合并，只提交账户 ID 以及 `access_token`、`refresh_token`、`id_token`，不会提交分组、代理、并发、优先级、倍率或 `extra`。更新前后都会复核账号标识和自动化归属标记，更新后还会核对提交的凭据是否实际写入；单账户失败不会中断后续账户。该入口同步输入已有 Token，不调用 OpenAI OAuth 接口换取新授权。

## Codex 指纹收敛

本模块导入的 OpenAI OAuth 账户通过 `defaults.extra.codex_fingerprint_mode` 选择模式；目录、邮箱和兑换码配置可按原有继承规则覆盖 `extra` 中的该字段。以下枚举依据 Sub2API `v0.2.15` 的[实现代码](https://github.com/Wei-Shaw/sub2api/blob/v0.2.15/backend/internal/service/openai_codex_fingerprint.go)，未设置或非法值按 `off` 处理。

| 配置值 | 界面选项 | 收敛行为 |
| --- | --- | --- |
| `off` | 关闭（透传，默认） | 关闭本层收敛，保留设备、会话和线程区分；网关原有的账户隔离和身份规范化仍执行。 |
| `device` | 仅设备 | 统一为账户固定安装标识，保留会话和线程区分。 |
| `session` | 设备+会话 | 统一安装和会话标识，按原始客户端会话派生线程；原始会话标识缺失时回退到统一线程。 |
| `full` | 完全收敛 | 统一安装、会话和线程标识，多个客户端共享同一线程标识。 |

`session` 和 `full` 还会调整窗口标识、为每次请求生成新的轮次标识；仅当根级 `prompt_cache_key` 等于请求体收敛前的会话标识时，才改写该缓存键。修改模板不会覆盖已有 ProgramData 配置，已有账户需经导入或增量设置同步后应用；Token 刷新不提交这些设置。

这些模式用于多人共享账户时减少上游可见的设备、会话标识数量。单人使用通常保持 `off`；多人共享且需要合并设备标识时，可先选改写较少的 `device`，`session/full` 按账户实测选择。相同安装标识不等于同一个人，也不统一出口 IP、并发或使用节奏；上游注释记录过收敛后额度缩水的报告，不能保证额度或稳定性改善。
