# Sub2API 模块

使用 Python 读取标准化的 `sub2api-accounts.json`，调用 Sub2API Admin API，完成管理员登录、分组和代理校验、账号幂等导入及变更字段显示。总流程中的来源可以是 Redeem 生成的 JSON，也可以是 `input\accounts.txt` 直接账户文本；两者先由 Normalize 统一格式。Cockpit 本地账号读取只保留为备选入口。只允许配置的本机回环地址，错误信息不会输出响应正文或凭据。

账户配置支持 `defaults` 和按邮箱的 `accounts`，规则见上级 README。本模块直接读取 JSON 或 Cockpit 账户时应用默认值和邮箱覆盖。生成的 Sub2API 输入在每个账户的 `config` 中保留最终设置，与 `credentials` 分开保存；单独重跑该 JSON 会使用相同的分组和设置。增量中仅配置变化的记录带有 `settings_only: true`，使用账户更新接口并保留服务器已有凭据及未配置的 `extra` 键；单独重跑这些记录仍只更新设置，不调用凭据导入接口，也不会重建不存在的账户。所有账户的设置、分组和代理会在第一个账户写入前完成校验。

默认情况下，没有 `extra.account_pipeline_managed=account-pipeline-v1` 标记的已有账户会被视为手动账户并跳过。需要一次性接管当前输入中精确匹配的已有账户时，在本机配置的 `defaults` 中设置 `claim_existing_accounts` 为 `true`；导入会保留账户 ID，并通过正常更新请求写入归属标记和当前配置。完成认领后建议恢复为 `false`，继续保护以后新增的手动账户。

单独运行：

```bat
run.bat <sub2api-accounts.json> --config <配置文件>
import-from-cockpit-tools.bat [--what-if]
```

`run.bat` 用于导入 JSON；`import-from-cockpit-tools.bat` 才会读取当前 Windows 用户的 Cockpit Tools 加密账号，需要安装 `requirements.txt` 中的 `cryptography` 包。后者不会被总流程自动调用。

总目录的 `incremental.bat` 会使用本模块只读查询带有 `extra.account_pipeline_managed=account-pipeline-v1` 归属标记的账号，用于建立首次增量基线和过滤手动账户。输入中减少的账号会把 ID、邮箱和原因写入 `..\cache\results\sub2api-pending-deletions.txt` 供手动处理；本模块不会调用删除接口，也不会按模糊名称或未知 ID 删除。没有归属标记的手动中转账户会跳过，不会被更新或写入工具快照。

总目录的 `refresh-tokens.bat` 复用本模块的凭据专用更新。它逐账号调用 `bulk-update` 的凭据键级合并，只提交账户 ID 以及 `access_token`、`refresh_token`、`id_token`，不会提交分组、代理、并发、优先级、倍率或 `extra`。更新前后都会复核账号标识和自动化归属标记。
