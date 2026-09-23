# Sub2API 模块

使用 Python 读取标准化的 `sub2api-accounts.json`，调用 Sub2API Admin API，完成管理员登录、分组和代理校验、账号幂等导入及变更字段显示。总流程中的来源可以是 Redeem 生成的 JSON，也可以是 `input\accounts.txt` 直接账户文本；两者先由 Normalize 统一格式。Cockpit 本地账号读取只保留为备选入口。只允许配置的本机回环地址，错误信息不会输出响应正文或凭据。

单独运行：

```bat
run.bat <sub2api-accounts.json> --config <配置文件>
import-from-cockpit-tools.bat [--what-if]
```

`run.bat` 用于导入 JSON；`import-from-cockpit-tools.bat` 才会读取当前 Windows 用户的 Cockpit Tools 加密账号，需要安装 `requirements.txt` 中的 `cryptography` 包。后者不会被总流程自动调用。

总目录的 `incremental.bat` 会使用本模块只读查询带有 `extra.account_pipeline_managed=account-pipeline-v1` 归属标记的账号，用于建立首次增量基线和过滤手动账户。输入中减少的账号会把 ID、邮箱和原因写入 `..\cache\results\sub2api-pending-deletions.txt` 供手动处理；本模块不会调用删除接口，也不会按模糊名称或未知 ID 删除。没有归属标记的手动中转账户会跳过，不会被更新或写入工具快照。
