# Codex 账号配置化导入

适用于把 CLI Proxy API 的 Codex Auth JSON 导入 Sub2API 0.2.3。脚本通过 Sub2API Admin API 创建或更新账号，同时设置分组、代理、并发、优先级、倍率和 Codex 高级选项；不直接写 PostgreSQL。

Sub2API 的普通数据包导入不会随账号绑定分组。数据包里没有明确填写的代理、倍率和高级选项也会使用默认值。因此，需要一次设置完整配置时，应使用本脚本调用原生 Codex Session 导入接口。

## 配置与输入选择

未显式传入 `-ConfigPath` 时，脚本按以下顺序选择配置：

1. `C:\ProgramData\Sub2API\codex-account-import.json` 自定义配置。
2. 工具目录内的 `config.example.json` 模板。

当前模板使用分组 `西郊-gpt`、代理 `Verge`、并发 `3`、优先级 `50`、倍率 `0.1` 及项目所需的 Codex 高级选项。需要长期自定义时，把模板复制到上述 ProgramData 路径后修改；需要临时使用另一份配置时，传入 `-ConfigPath D:\path\to\custom-config.json`。

账号输入按以下顺序选择：

1. 命令行 `-InputPath`；拖入 BAT 时由 BAT 自动传入被拖入文件的绝对路径。
2. 配置中的可选 `input_path`。

两处都没有提供输入文件时，脚本停止。配置中的相对 `input_path` 以配置文件所在目录为基准；命令行相对路径以当前工作目录为基准。

管理员账号与密码从 `runtime_env_path` 指定的 `.env` 读取，不写入导入配置或日志。`sub2api_url` 只接受本机回环地址，避免把管理员密码发送到其他主机。

## 使用方法

日常使用时，把一个账号 JSON 文件拖到 `drop-json-to-import.bat` 上即可正式导入。窗口会保留导入结果；一次只能拖入一个文件。

先校验输入、管理员登录、分组和代理，不创建或修改账号：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\Tools\codex-account-import\import-codex-accounts.ps1 `
  -InputPath D:\path\to\codex-auth.json -WhatIf
```

校验通过后去掉 `-WhatIf` 正式导入。脚本为每条记录调用 `/api/v1/admin/accounts/import/codex-session`，并固定启用 `update_existing=true`；匹配到已有 Codex 账号时更新该账号，不重复创建。

输入文件支持以下三种结构：

| 结构 | 示例形态 | 读取方式 |
| --- | --- | --- |
| 单条 Auth JSON | `{ "type": "codex", ... }` | 作为一个账号导入 |
| Auth JSON 数组 | `[{ "type": "codex", ... }]` | 数组中每条记录作为一个账号导入 |
| Sub2API 数据包 | `{ "accounts": [{ "credentials": {...} }] }` | 读取每个 `accounts[].credentials` |

每条凭据必须包含 `access_token` 或 `accessToken`。使用当前邮箱命名规则时还必须包含 `email`。脚本不会输出密码、Token、TOTP 或邮箱。

## JSON 顶层参数

“省略时”表示自定义配置中没有该字段时的代码缺省行为；它可能与当前模板值不同。

| 参数 | 类型 | 当前模板值 | 省略时 | 作用与限制 |
| --- | --- | --- | --- | --- |
| `runtime_env_path` | 字符串 | `C:\ProgramData\Sub2API\runtime\.env` | 同模板 | Sub2API 运行环境文件路径。必须包含 `ADMIN_EMAIL` 和 `ADMIN_PASSWORD`；`sub2api_url` 留空时还会读取可选的 `SERVER_PORT`。 |
| `sub2api_url` | 字符串 | `http://127.0.0.1:18080` | 使用 `http://127.0.0.1:<SERVER_PORT>`；没有 `SERVER_PORT` 时端口为 `18080` | Sub2API 服务根地址，不要附加 `/api/v1`。只允许 `http` 或 `https` 的本机回环地址，不能包含用户信息、查询参数或片段。 |
| `source_type` | 字符串 | `codex` | `codex` | 只导入凭据中 `type` 与该值相同的记录，比较不区分大小写。设为 `""` 可关闭类型过滤。 |
| `input_path` | 字符串 | 未配置 | 无输入备用值 | 可选账号文件路径。`-InputPath` 优先于该字段；拖入 BAT 时无需填写。 |
| `name_prefix` | 字符串 | `""` | `""` | 留空时使用每条凭据的 `email` 作为账户名称，缺少邮箱即停止。填写后改为“前缀 + 连续编号”。 |
| `name_start` | 整数 | `1` | `1` | 连续编号起始值，只在 `name_prefix` 非空时使用，不能小于 `0`。 |
| `name_width` | 整数 | `3` | `3` | 连续编号的最小位数，只在 `name_prefix` 非空时使用；范围为 `1` 到 `99`。例如 `3` 生成 `001`。 |
| `group_names` | 字符串数组 | `["西郊-gpt"]` | `[]` | 导入后绑定的分组名称，可填写多个。名称按不区分大小写的方式精确匹配，并且必须唯一、启用且属于 OpenAI 平台；空数组表示不绑定分组。 |
| `proxy_name` | 字符串 | `Verge` | `""` | 导入后绑定的代理名称。名称必须唯一且代理已启用；留空表示直连。 |
| `concurrency` | 整数 | `3` | `3` | 写入账户并发数，必须大于 `0`。 |
| `priority` | 整数 | `50` | `50` | 写入账户调度优先级，必须大于 `0`；实际调度含义由 Sub2API 决定。 |
| `rate_multiplier` | 数字 | `0.1` | `1` | 直接写入账户最终计费倍率，必须大于或等于 `0`。它不是 rate-sync 中的充值折扣 `recharge_discounts`。 |
| `load_factor` | 整数或 `null` | `null` | `null` | 非空时必须大于 `0` 并写入账户负载因子；`null` 时不发送该字段，由 Sub2API 使用默认行为。 |
| `notes` | 字符串 | `""` | `""` | 写入账户备注。 |
| `expires_at` | 字符串 | `""` | `""` | 可选账户到期时间，使用带时区的 ISO 8601 时间，例如 `2026-12-31T23:59:59+08:00`；留空时不设置。不要填写短期 Access Token 的过期时间。 |
| `auto_pause_on_expired` | 布尔值 | `true` | `false` | 是否在账户达到 `expires_at` 后自动暂停。未设置 `expires_at` 时不会因该选项自行产生到期时间。 |
| `extra` | JSON 对象 | 见下表 | `{}` | 原样提交给 Sub2API 的账户高级选项。Sub2API 可能在保存后补充自己的运行状态字段。 |

## `extra` 高级参数

| 参数 | 类型 | 当前模板值 | 作用 |
| --- | --- | --- | --- |
| `codex_cli_only` | 布尔值 | `true` | 仅对 OpenAI OAuth 账号生效。开启后只允许 Codex 官方客户端家族访问；其他客户端可能被拒绝。需要供其他客户端使用时设为 `false`。 |
| `codex_fingerprint_mode` | 字符串 | `full` | Codex 指纹模式，可使用 `off`、`device`、`session` 或 `full`；当前使用完整模式。 |
| `openai_long_context_billing_enabled` | 布尔值 | `true` | 启用 OpenAI 长上下文计费选项。 |
| `openai_passthrough` | 布尔值 | `true` | 启用 OpenAI 请求透传选项。 |

模板中的 `codex_cli_only=true` 是为了与当前已导入账号保持一致，不是通用推荐。如果还要允许通过 app-server 协议接入的客户端，需要另外了解 Sub2API 的 `codex_cli_only_allow_app_server` 选项。

这些高级参数由 Sub2API 0.2.3 解释。自定义配置可以删除不需要显式设置的键，但删除后将由 Sub2API 的默认行为决定。
