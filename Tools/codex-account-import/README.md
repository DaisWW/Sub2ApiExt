# Codex 账号配置化导入

适用于把 CLI Proxy API 的 Codex Auth JSON 或 Sub2API 数据包导入 Sub2API 0.2.3。脚本通过 Sub2API Admin API 创建或更新账号，同时设置分组、代理、并发、优先级、倍率和 Codex 高级选项；不直接写 PostgreSQL。

Sub2API 的普通数据包导入不会随账号绑定分组。数据包里没有明确填写的代理、倍率和高级选项也会使用默认值。因此，需要一次设置完整配置时，应使用本脚本调用原生 Codex Session 导入接口。

## 配置与输入选择

未显式传入 `-ConfigPath` 时，脚本按以下顺序选择配置：

1. `C:\ProgramData\Sub2API\codex-account-import.json` 自定义配置。
2. 工具目录内的 `config.example.json` 模板。

当前模板沿用现有账号的分组 `西郊-gpt`、代理 `Verge`、并发 `3`、优先级 `50`、倍率 `0.1` 及 Codex 高级选项。需要长期自定义时，把模板复制到上述 ProgramData 路径后修改；需要临时使用另一份配置时，传入 `-ConfigPath D:\path\to\custom-config.json`。

`group_names` 是数组，同一批导入账号会同时绑定数组中的每个分组。例如：

```json
"group_names": ["西郊-gpt", "策略-gpt-低价"]
```

这表示每个导入账号都进入两个组，不会把账号平均拆分。若要让不同账号批次进入不同组，可以在一个配置里使用 `batches`，每个批次单独指定输入文件和分组：

```json
"batches": [
  {
    "input_path": "accounts-west.json",
    "group_names": ["西郊-gpt"]
  },
  {
    "input_path": "accounts-strategy.json",
    "group_names": ["策略-gpt-低价"]
  }
]
```

批次中的其他字段默认继承顶层配置；每个批次都必须明确填写 `group_names`，避免误继承顶层分组。如果需要让同一批账号进入多个组，可在该批次的 `group_names` 中填写多个名称。配置使用 `batches` 时，每个批次的路径相对于配置文件所在目录。若同时传入 `-InputPath`（包括拖入 BAT），命令行输入会优先按单批次处理，并使用顶层配置；没有 `batches` 时，原来的单批次配置和拖入 BAT 用法保持不变。

单批次配置的账号输入按以下顺序选择：

1. 命令行 `-InputPath`；拖入 BAT 时由 BAT 自动传入被拖入文件的绝对路径。
2. 配置中的可选 `input_path`。

两处都没有提供输入文件时，脚本停止。配置中的相对 `input_path` 以配置文件所在目录为基准；命令行相对路径以当前工作目录为基准。使用 `batches` 时，输入文件由每个批次的 `input_path` 提供。

管理员账号与密码从 `runtime_env_path` 指定的 `.env` 读取，不写入导入配置或日志。`sub2api_url` 只接受本机回环地址，避免把管理员密码发送到其他主机。

## 使用方法

日常使用时，把一个账号 JSON 文件拖到 `drop-json-to-import.bat` 上即可正式导入。窗口会保留导入结果；一次只能拖入一个文件。

多批次配置使用 `-ConfigPath` 执行，例如：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\Tools\codex-account-import\import-codex-accounts.ps1 `
  -ConfigPath C:\ProgramData\Sub2API\codex-account-import.json -WhatIf
```

先校验输入、管理员登录、分组和代理，不创建或修改账号；`-WhatIf` 不会调用导入接口验证每条凭据能否被接受：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File .\Tools\codex-account-import\import-codex-accounts.ps1 `
  -InputPath D:\path\to\codex-auth.json -WhatIf
```

校验通过后去掉 `-WhatIf` 正式导入。脚本为每条记录调用 `/api/v1/admin/accounts/import/codex-session`，并固定启用 `update_existing=true`；凭据身份匹配到已有 Codex 账号时更新，否则新建。更新已有账号不会修改名称和备注。记录逐条提交，中途失败时此前成功的记录不会回滚。

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
| `batches` | 对象数组 | 未配置 | 使用单批次模式 | 多批次导入；每项必须填写 `input_path` 和 `group_names`，并可填写 `source_type`、`name_prefix`、`name_start`、`name_width` 覆盖顶层值。若传入 `-InputPath`，则按单批次处理并优先使用命令行文件。 |
| `name_prefix` | 字符串 | `""` | `""` | 新建账号时，留空使用每条凭据的 `email` 命名，缺少邮箱即报错；填写后改为“前缀 + 连续编号”。更新已有账号不修改名称。 |
| `name_start` | 整数 | `1` | `1` | 连续编号起始值，只在 `name_prefix` 非空时使用，不能小于 `0`。 |
| `name_width` | 整数 | `3` | `3` | 连续编号的最小位数，只在 `name_prefix` 非空时使用；范围为 `1` 到 `99`。例如 `3` 生成 `001`。 |
| `group_names` | 字符串数组 | `["西郊-gpt"]` | `[]` | 导入时指定的分组名称，可填写多个；同一批每个账号都会绑定全部列出的分组。名称按不区分大小写的方式精确匹配，并且必须唯一、启用且属于 OpenAI 平台。空数组时，新建账号可能绑定 `openai-default`，更新已有账号则保留原分组。批次模式下每个批次必须明确填写。 |
| `proxy_name` | 字符串 | `Verge` | `""` | 导入时指定的代理名称。名称必须唯一且代理已启用；留空时新建账号直连，更新已有账号保留原代理。 |
| `concurrency` | 整数 | `3` | `3` | 写入账户并发数，必须大于 `0`。 |
| `priority` | 整数 | `50` | `50` | 写入账户调度优先级，必须大于 `0`；数值越小优先级越高。 |
| `rate_multiplier` | 数字 | `0.1` | `1` | 导入时写入账户计费倍率，必须大于或等于 `0`（`0` 表示不计费）；之后可能被 rate-sync 的账户同步覆盖。它不是充值折扣 `recharge_discounts`。 |
| `load_factor` | 整数或 `null` | `null` | `null` | 非空时必须大于 `0` 并写入账户负载因子；`null` 时不发送，新建账号使用 Sub2API 默认值，更新已有账号保留原值。 |
| `notes` | 字符串 | `""` | `""` | 新建账号时写入备注；更新已有账号不修改备注。 |
| `expires_at` | 字符串 | `""` | `""` | 可选账户到期时间，使用带时区的 ISO 8601 时间，例如 `2026-12-31T23:59:59+08:00`；脚本转换成 Unix 秒提交。无 `refresh_token` 时，Sub2API 取该值与可解析的 Access Token 到期时间中更早者；两者都没有则导入失败。有 `refresh_token` 时，不要把短期 Access Token 的过期时间填作账号到期时间。 |
| `auto_pause_on_expired` | 布尔值 | `true` | `false` | 账号到期后是否自动暂停。无 `refresh_token` 时，Sub2API 会强制启用，即使这里填写 `false`。 |
| `extra` | JSON 对象 | 见下表 | `{}` | 提交账户高级选项。Sub2API 会与凭据解析出的选项合并；更新已有账号时还会与原有 `extra` 合并。 |

若导入凭据没有 `refresh_token`，但匹配到的已有账号有，Sub2API 会保留原有续期凭据，本次不会更新该账号的到期时间和自动暂停设置。

## `extra` 高级参数

| 参数 | 类型 | 当前模板值 | 作用 |
| --- | --- | --- | --- |
| `codex_cli_only` | 布尔值 | `true` | 仅对 OpenAI OAuth 账号生效。开启后只允许 Codex 官方客户端家族访问；其他客户端可能被拒绝。需要供其他客户端使用时设为 `false`。 |
| `codex_fingerprint_mode` | 字符串 | `full` | 收敛 Codex 设备/会话标识，可使用 `off`、`device`、`session` 或 `full`。Sub2API 默认 `off`；模板沿用现有账号的 `full`，部分账号开启后可能出现额度缩水，按实测选择。 |
| `openai_long_context_billing_enabled` | 布尔值 | `true` | 仅当上游会按模型阈值收取 OpenAI API 长上下文费率时才应开启；Sub2API 默认关闭，模板沿用现有账号配置。 |
| `openai_passthrough` | 布尔值 | `true` | 自动透传 OpenAI 请求与响应，仅替换认证，并保留计费、并发、审计及必要的安全过滤。 |

模板中的 `codex_cli_only=true` 是为了与当前已导入账号保持一致，不是通用推荐。如果还要允许通过 app-server 协议接入的客户端，需要另外了解 Sub2API 的 `codex_cli_only_allow_app_server` 选项。

这些高级参数由 Sub2API 0.2.3 解释。自定义配置删除某个键时，新建账号由 Sub2API 决定默认行为；更新已有账号会保留原有 `extra` 中的键，删除配置项不能清除旧值。
