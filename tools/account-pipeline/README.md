# 账号流水线

总入口 `run.bat` 启动 Python 编排器。完整流程支持两种输入：一行一个卡密的 TXT，以及可连续放置多个账户 JSON 对象的账户文本。两个输入同时存在时会合并、去重后生成统一数据，再分别导入 Sub2API 和 Cockpit；全量模式每次都会把本次合并后的全部账号交给两个导入模块，不按增量快照跳过。`incremental.bat` 导入新增、授权更新或账户设置发生变化的账号。`refresh-tokens.bat` 只比较并刷新变化的 token，不提交账户设置。输入中减少的账号只记录待手动处理清单，不会自动删除。编排器依次调用四个独立模块：

1. `redeem`：提交卡密，按提取/检测阶段显示逐卡结果，覆盖写入固定结果 TXT，下载 ZIP 并安全解压。
2. `normalize`：读取解压目录中的账号 JSON 和可选账户文本，校验来源冲突、去重并生成两个目标输入文件。
3. `sub2api`：读取标准化的 `sub2api-accounts.json`，调用 Sub2API Admin API，按账号执行幂等导入并显示变更字段。
4. `cockpit`：通过回环一次性 HTTP 链接把标准化 JSON 交给 Cockpit Tools。

## 使用

```bat
run.bat
run.bat D:\input\redeem-codes.txt --skip-cockpit
run.bat --codes-file D:\input\redeem-codes.txt --accounts-file D:\input\accounts.txt
run.bat D:\input\accounts.txt --skip-sub2api --skip-cockpit
run.bat --dry-run
incremental.bat
refresh-tokens.bat
```

不传输入参数时读取 `input` 根目录和一层子目录：卡密文件固定为 `redeem-codes.txt`，账户文件使用 `accounts*.txt`、`accounts*.json` 或 `accounts*.jsonl`，示例文件会忽略，不递归读取更深目录。可以把账户分散保存到多个文件，每个账户只需保存一份凭据。目录名不再决定分组，目录 `config.json` 不再生效。

卡密文件一行一个卡密，空行和 `#` 开头的行会忽略；卡密后可用空格附加注释。重复卡密只提交一次。账户文本可以是单个对象、数组、JSONL 或连续的完整 JSON 对象，中间允许空行。首次使用可把 `input\accounts.example.txt` 复制到 `input\accounts.txt`，再填写真实账户对象。

位置参数会自动识别卡密或账户文本，并只处理拖入的这一种来源。使用 `--codes-file`、`--accounts-file` 时只读取明确指定的文件，两个选项一起传才会合并这两种来源。所有输入方式使用同一份账户配置。`--dry-run` 验证本地输入和配置，不访问兑换站或导入服务；兑换码对应的邮箱及远端分组、代理仍需在实际运行时校验。

## 默认配置、邮箱覆盖和兑换码覆盖

账户凭据保存在输入文件，设置集中保存在 Sub2API 主配置中。连接地址、登录环境和来源类型为顶层字段，账户设置放在 `defaults`；旧配置的顶层账户字段仍可作为默认值使用。以下只展示账户设置部分，完整示例见 `sub2api/config.example.json`：

```json
{
  "defaults": {
    "group_names": ["西郊-gpt"],
    "concurrency": 3,
    "priority": 50,
    "rate_multiplier": 0.1,
    "extra": {
      "codex_cli_only": true,
      "codex_fingerprint_mode": "full",
      "openai_long_context_billing_enabled": true,
      "openai_passthrough": true
    }
  },
  "accounts": {
    "user@example.com": {
      "group_names": ["西郊-gpt", "西郊-gpt-cursor"],
      "extra": { "codex_cli_only": false }
    }
  },
  "redeem_codes": {
    "PLUS-EXAMPLE": {
      "group_names": ["西郊-gpt", "西郊-gpt-cursor"],
      "extra": { "codex_cli_only": false }
    }
  }
}
```

`accounts` 和 `redeem_codes` 均可省略。已知邮箱时写 `accounts`；兑换前不知道邮箱时写 `redeem_codes`，同时将该卡密填入 `redeem-codes.txt`。配置条目本身不会产生输入，也不会触发兑换。邮箱去除首尾空格并忽略大小写；兑换码去除首尾空格后精确匹配。

覆盖优先级为 **默认值 → 兑换码 → 邮箱**。普通字段覆盖，未写字段继承；`extra` 按键覆盖。`group_names` 缺省时继承，显式填写时就是最终完整列表，空数组清除分组；需要同时加入两个分组时直接列出两个名称。分组必须是已存在且启用的 OpenAI 分组。`codex_cli_only` 是账户级设置，对该账户所在的所有分组生效。

兑换接口的逐卡结果提供卡密与邮箱的对应关系，manifest 只新增卡密 SHA-256 摘要与邮箱的关联。已配置的成功卡密若缺少可识别邮箱的对应关系，会在导入前停止；邮箱已知但下载账户无效时只记录该账户失败。失败卡密仍按兑换警告处理，不阻塞其他成功输入。同一账户由多个卡密提供设置时，卡密的分组列表合并去重；其他字段或同一 `extra` 键不同则隔离该账户，邮箱配置中明确设置的字段优先。凭据不一致的重复账户也会隔离，不按文件顺序选择。

便于查看的兑换码与邮箱对应关系另外保存在 `input\redeem-code-email-map.json`，不受账号状态限制并累计保留，详见 `redeem/README.md`。这个生成文件不会被作为输入读取。

删除邮箱覆盖条目会恢复兑换码设置或默认值，不会删除账户；移除一个分组只需修改最终分组数组。账户从全部输入中移除后仍只记录待手动处理清单，不自动删除远端账户。

全量和增量导入都按上述规则生成每账户设置。增量通过配置指纹识别变化，只改配置时调用 Admin API 更新设置，保留服务器凭据和未配置的 `extra` 键，也不会向 Cockpit 提交旧 Token。旧快照没有指纹或首次建立基线时，已带工具归属标记的账户同步设置，未匹配账户仍进入逐账户导入和归属检查。Token 同步使用相同输入，但只更新凭据。

仅同步设置时，`proxy_name: ""`、`load_factor: null`、`expires_at: ""` 会清除远端对应的旧值。全量凭据导入也会落实名称、备注和完整分组列表；纯 Access Token 账户保留上游导入接口计算的到期和自动暂停策略。

各模块也能单独运行：

```bat
redeem\run.bat [卡密文件]
normalize\run.bat --data-dir <解压目录> --output-dir <输出目录>
normalize\run.bat --input-file <账户文本> --output-dir <输出目录>
sub2api\run.bat <sub2api-accounts.json> --config <配置文件>
sub2api\import-from-cockpit-tools.bat [--what-if]
cockpit\run.bat <cockpit-accounts.json>
incremental.bat [拖入卡密或账户文本]
refresh-tokens.bat [拖入卡密或账户文本]
```

根目录的 `run.bat` 统一查找 Python，由 `launch.py` 选择目标脚本并传递业务参数；全量、增量、Token 刷新都进入同一份 `run.py` 编排实现，模块 BAT 只传入目标脚本和参数。

三个模式共用输入读取、空来源处理、本地凭据冲突检查和错误日志；`--dry-run` 也执行相同的本地冲突检查，但不会兑换卡密或调用导入服务。三种模式只保留账号选择、写入字段及快照规则的业务差异。同一运行目录每次只允许一个主流水线执行，重复启动会报错；进程退出后运行锁自动释放。

单账户缺少凭据、配置错误、输入冲突、远端匹配或导入失败都会隔离该账户，继续处理其他有效账户。Sub2API 完整更新并复核成功的账户才交给 Cockpit，并写入成功快照；部分失败的运行记录为 `partial_failure`，退出码为 `1`，表示处理完成后仍有失败项。全部账户无效时也保存失败记录，不执行导入。已有账户失败时保留旧 ID 和元数据；实际凭据导入失败会安排重试，输入恢复本身不会触发旧 Token 重新写入。设置失败保留旧配置指纹，重跑仍只更新设置。坏输入文件会单独记录并跳过；无法确定其中账户身份时，本轮暂停输入减少判断，保留旧快照，避免误列清理项。管理员登录、公共配置结构、共享服务或状态文件错误仍会停止流程。Cockpit 的外部链接只能确认数据已投递，无法逐账户确认其应用内导入结果。

## Token 刷新

`refresh-tokens.bat` 使用与全量流程相同的卡密和账户文本输入。它先兑换、合并和标准化，再与 `cache\state\token-snapshot.json` 中的明文 `access_token`、`refresh_token`、`id_token` 比较。没有快照的首次运行会把全部当前账号视为变化；以后只处理 token 内容发生变化的账号。

这里的“刷新”是同步输入中已有的 Token，工具不会调用 OpenAI OAuth 接口换取新 Token，也不会因本地判断授权过期而过滤账户。实际导入失败按单账户记录。

Sub2API 更新请求只包含账户 ID 和变化账号的三个 token 字段，通过凭据键级合并保留分组、代理、并发、优先级、倍率、名称、备注及其他已有凭据字段。此模式不读取账户设置覆盖，无关的设置错误不会拦截凭据同步。只有带 `extra.account_pipeline_managed=account-pipeline-v1` 标记的账号可以同步；找不到的账号、手动账号和更新失败的账号只记录自身失败，其他账号继续。Sub2API 成功项会交给 Cockpit；投递完成后仅更新这些成功项的明文快照，失败项下次仍会重试。日志和 manifest 只保存数量、账号标识、脱敏原因和状态，不保存 token 内容。

## 增量同步

`incremental.bat` 固定读取与完整流程相同的输入。第一次运行建立安全基线，并查询当前带有本工具归属标记的 Sub2API 账号，同步这些账号的设置；未匹配账户仍进入导入和归属检查，新增或远端已删除的账户不会被基线丢弃。兑换结果明确提示“授权已更新”的已归属账号仍重新导入凭据。以后导入新增账号、授权更新账号以及配置变化的账号，未变化账号跳过。安全基线检查账户归属，不探测 OAuth 授权是否失效。输入中减少的账号会输出日志并写入待手动处理清单，工具不会调用任何删除接口。新增和授权更新账号按“Sub2API 后 Cockpit”的顺序导入；只有配置变化的账号仅同步 Sub2API 设置。

Sub2API 导入成功的账号会在 `extra.account_pipeline_managed` 写入固定值 `account-pipeline-v1`。同邮箱但没有这个标记的中转账户默认视为手动账户，脚本会显示“跳过手动账户”，不会更新或写入增量快照。需要认领旧版导入账户时，可在本机 Sub2API 配置的 `defaults` 中临时设置 `claim_existing_accounts: true`；它只对本轮输入中精确匹配的账户生效，完成一次认领后应恢复为 `false`。兑换来源和 `accounts.txt` 来源都属于脚本主动导入范围；修改输入文件范围时会重新建立安全基线，不会据此删除旧账号。旧版没有归属标记的增量快照也只会触发安全基线。

把卡密行改成 `# PLUS-...` 后，它不再参与本轮输入；下一次增量运行会把对应的减少账号写入待手动处理清单。空白或全部被注释的卡密文件会跳过兑换；增量模式允许空输入，以便记录账户减少，全量模式要求至少一个有效账户或卡密。没有归属标记的账号也不会发送到 Cockpit 自动导入。

Cockpit Tools 当前公开的外部链接只支持导入，没有删除命令。脚本会把输入中减少的邮箱累计写入 `cache\results\cockpit-pending-deletions.txt`，供你在 Cockpit Tools 中手动处理，不会直接修改其加密存储。为保证一个快照代表两个目标的同一状态，增量模式不接受 `--skip-sub2api` 或 `--skip-cockpit`。

输入中减少账号时，脚本会把 ID、邮箱和原因写入 `cache\results\sub2api-pending-deletions.txt`（不含凭据），并继续导入本轮新增或授权更新账号；工具不会调用删除接口，你可以按清单手动处理。账号重新出现在当前输入时，会从待处理清单移除。

旧快捷入口对应的新位置是：`redeem\redeem-account-import.bat`、`cockpit\drop-json-to-import.bat` 和 `sub2api\import-from-cockpit-tools.bat`。最后一个只读取 Cockpit Tools 本地账号，是 Sub2API 的备选来源，不参与总流程。BAT 只负责找到 Python、传递参数和显示退出码，业务逻辑全部在 `.py` 文件中。

## 运行目录

默认运行目录是工具目录下被 Git 忽略的 `cache`，即 `tools\account-pipeline\cache`，可在本目录创建被 gitignore 的 `config.json` 覆盖：

```json
{
  "runtime_dir": "cache",
  "sub2api_config": "",
  "cockpit_wait_seconds": 60
}
```

每次运行生成独立目录：

```text
<runtime_dir>/
├─ runs/<run-id>/
│  ├─ redeem/                 ZIP 和解压数据
│  ├─ normalized/             两个导入目标 JSON
│  ├─ incremental/            本次新增、输入减少记录 JSON
│  ├─ redeem-manifest.json    兑换元数据
│  └─ manifest.json           阶段状态和退出码
├─ state/incremental.json     增量快照（账号标识、Sub2API ID 和配置指纹）
├─ state/token-snapshot.json  token 明文比较快照
├─ logs/pipeline-*.log        流水线日志
└─ results/
   ├─ redeem-result.txt       最近一次逐卡结果（UTF-8 BOM TSV）
   ├─ input-conflicts.txt     最近一次输入源冲突检查结果（不含 token）
   ├─ cockpit-pending-deletions.txt  Cockpit 待手动处理账号
   └─ sub2api-pending-deletions.txt  Sub2API 待手动处理账号
```

manifest 保存路径、数量、任务号、阶段状态、授权更新账号标识（邮箱）和账号来源标识（邮箱），不保存卡密、Token、密码或账号凭据。`normalized` 下的 JSON 含凭据，只保存在本机运行目录。

缓存和过渡文件的实际位置如下：

| 内容 | 默认位置 |
| --- | --- |
| 下载的 ZIP | `tools\account-pipeline\cache\runs\<run-id>\redeem\` |
| ZIP 解压出的账号 JSON | `tools\account-pipeline\cache\runs\<run-id>\redeem\data\` |
| 给 Sub2API 和 Cockpit 的标准化 JSON | `tools\account-pipeline\cache\runs\<run-id>\normalized\` |
| 本次运行 manifest | `tools\account-pipeline\cache\runs\<run-id>\manifest.json` |
| 最近一次卡密结果 | `tools\account-pipeline\cache\results\redeem-result.txt` |
| 最近一次输入冲突结果 | `tools\account-pipeline\cache\results\input-conflicts.txt` |
| token 明文比较快照 | `tools\account-pipeline\cache\state\token-snapshot.json` |
| Cockpit 待手动处理清单 | `tools\account-pipeline\cache\results\cockpit-pending-deletions.txt` |
| Sub2API 待手动处理清单 | `tools\account-pipeline\cache\results\sub2api-pending-deletions.txt` |
| 增量状态和日志 | `tools\account-pipeline\cache\state\`、`tools\account-pipeline\cache\logs\` |

这些缓存不写入 Git 工作区。若需要换到其他目录，可以在命令行指定 `--runtime-dir D:\Sub2API-cache`，或在被忽略的 `config.json` 中设置 `runtime_dir`。`input` 中的卡密和账户文件是本机输入，已加入 Git 忽略规则；示例文件可以提交。账户配置保存在 ProgramData，不依赖 Git 工作区。

## Sub2API 配置

复制 `sub2api\config.example.json` 到本机配置位置后按实际环境修改。默认查找顺序是：`C:\ProgramData\Sub2API\account-pipeline\sub2api.json`、已有的 `C:\ProgramData\Sub2API\codex-account-import.json` 配置、模块内示例配置。配置中的 `runtime_env_path` 支持绝对路径和相对于配置文件的路径；环境文件提供 `ADMIN_EMAIL`、`ADMIN_PASSWORD`，不会写入日志。

## 输入边界和生成文件

主流程有两种独立外部输入：`redeem-codes.txt` 保存卡密，`accounts*` 文件保存已有账户 JSON 文本。账户文本中的每个对象必须是完整 JSON，但对象之间可以空行；支持单个对象、数组、JSONL 和连续对象。截图中常见的 `type: "oauth"` 且 `platform: "openai"` 会统一转换为 `type: "codex"`。

两种来源独立处理：卡密文件为空、只有空白或注释时，只要账户文本包含有效账号，就会跳过兑换并继续导入；账户文本为空时仍可处理卡密。全量模式下，两种来源都为空会报错；有失败输入时会保存失败记录。格式错误或包含重复 JSON 键的账户文件会隔离该文件，其他文件中的有效账户继续。

有卡密时先由 `redeem` 下载并解压；随后 `normalize` 同时读取解压目录和账户文本，按邮箱或账号 ID 去重，生成 `normalized\sub2api-accounts.json` 和 `normalized\cockpit-accounts.json`。同一账号在不同输入中完全相同可以合并；邮箱、`account_id` 或三个 token 字段不同时会覆盖写入 `cache\results\input-conflicts.txt`，隔离涉及的账户并继续其他账户，报告只列账号、来源和不同字段，不包含 token 内容。日志会列出兑换来源、账户文本来源、重复数和合并后的总数；这两个 JSON 是各自导入器的单独重跑输入。标准化要求每个账号有 `access_token` 和可识别的邮箱。

兑换结果中有失效卡密时，已下载的账号和 `accounts.txt` 仍会继续合并导入，异常只写入结果和日志，不改变流程成功状态；输入中减少的账号仍只记录待手动处理清单，不会自动删除。这个情况的运行记录会标记为 `success_with_redeem_warnings`。
