# Codex 账号配置化导入

适用于把 CLI Proxy API 的 Codex Auth JSON 导入 Sub2API 0.2.3。脚本通过 Sub2API Admin API 创建或更新账号，同时设置分组、代理、并发、优先级、倍率和 Codex 高级选项；不直接写 PostgreSQL。

Sub2API 的普通数据包导入不会随账号绑定分组。数据包里没有明确填写的代理、倍率和高级选项也会使用默认值。因此，需要一次设置完整配置时，应使用本脚本调用原生 Codex Session 导入接口。

先把 `config.example.json` 复制到 `C:\ProgramData\Sub2API\codex-account-import.json`，把分组、代理、倍率等示例值改为实际配置。管理员账号与密码从 `C:\ProgramData\Sub2API\runtime\.env` 读取，不写入导入配置或日志。`sub2api_url` 只接受本机回环地址，避免把管理员密码发送到其他主机。

日常使用时，把一个账号 JSON 文件拖到 `drop-json-to-import.bat` 上即可正式导入。BAT 会把拖入文件的路径传给脚本，因此配置中不需要 `input_path`。窗口会保留导入结果；一次只能拖入一个文件。

先校验输入、管理员登录、分组和代理，不创建或修改账号：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\Tools\codex-account-import\import-codex-accounts.ps1 `
  -ConfigPath C:\ProgramData\Sub2API\codex-account-import.json `
  -InputPath D:\path\to\codex-auth.json -WhatIf
```

校验通过后去掉 `-WhatIf` 执行导入。脚本按名称精确解析已有分组和代理；名称不存在、重复、未启用或平台不匹配时会停止。每条记录单独调用 `/api/v1/admin/accounts/import/codex-session`，并启用 `update_existing`，重复执行会更新匹配账号而不是再次创建。

常用配置：

| 字段 | 作用 |
| --- | --- |
| `input_path` | 可选备用输入路径；拖入 BAT 或使用 `-InputPath` 时不需要配置 |
| `name_prefix`、`name_start`、`name_width` | `name_prefix` 留空时直接使用源账号邮箱；填写前缀时生成连续账号名 |
| `group_names` | 导入时直接绑定的 OpenAI 分组名称，可填写多个 |
| `proxy_name` | 导入时直接绑定的现有代理名称；留空表示直连 |
| `concurrency`、`priority`、`rate_multiplier` | Sub2API 账号调度和计费参数 |
| `load_factor` | 可选负载因子；`null` 表示使用 Sub2API 默认值 |
| `expires_at`、`auto_pause_on_expired` | 可选账号到期时间及到期自动暂停；不要把短期 Access Token 到期时间当作账号到期时间 |
| `extra` | 原样传给 Sub2API 的账号高级选项 |

输入文件可以是单条 Auth JSON、Auth JSON 数组，或含 `accounts[].credentials` 的 Sub2API 数据包。脚本不会输出密码、Token、TOTP 或邮箱。
