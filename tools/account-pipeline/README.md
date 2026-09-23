# 账号流水线

总入口 `run.bat` 启动 Python 编排器。完整流程支持两种输入：一行一个卡密的 TXT，以及可连续放置多个账户 JSON 对象的账户文本。两个输入同时存在时会合并、去重后生成统一数据，再分别导入 Sub2API 和 Cockpit；全量模式每次都会把本次合并后的全部账号交给两个导入模块，不按增量快照跳过。`incremental.bat` 只导入新增或兑换结果标记为“授权已更新”的账号。`refresh-tokens.bat` 只比较并刷新变化的 token，不提交账户设置。输入中减少的账号只记录待手动处理清单，不会自动删除。编排器依次调用四个独立模块：

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

不传任何输入参数时分别读取 `input\redeem-codes.txt` 和 `input\accounts.txt`（存在才读取），适合一键同时处理两种来源。卡密文件一行一个卡密，空行和 `#` 开头的行会忽略；卡密后可用空格附加注释，注释会被忽略。账户文本可以是单个对象、数组、JSONL，或多个完整 JSON 对象连续放置，中间允许空行；不要求整个文件再包一层数组。位置参数会自动识别卡密或账户文本，并只处理拖入的这一种来源；使用任一 `--codes-file`、`--accounts-file` 时只读取明确指定的来源，两个选项一起传才会合并外部文件。`--dry-run` 只验证本地输入，不访问兑换站或导入服务。

首次使用可复制 `input\accounts.example.txt` 为 `input\accounts.txt`，再把真实账户对象替换进去；示例只使用占位令牌。

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

根目录的 `run.bat` 是唯一的 Python 启动实现；模块 BAT 只传入目标脚本和参数，因此根目录不会再有第二个公共启动 BAT。

## Token 刷新

`refresh-tokens.bat` 使用与全量流程相同的卡密和账户文本输入。它先兑换、合并和标准化，再与 `cache\state\token-snapshot.json` 中的明文 `access_token`、`refresh_token`、`id_token` 比较。没有快照的首次运行会把全部当前账号视为变化；以后只处理 token 内容发生变化的账号。

Sub2API 更新请求只包含账户 ID 和变化账号的三个 token 字段，通过凭据键级合并保留分组、代理、并发、优先级、倍率、名称、备注及其他已有凭据字段。只有带 `extra.account_pipeline_managed=account-pipeline-v1` 标记的账号可以刷新；找不到的账号和手动账号会写入日志并使本次运行返回失败，提醒先通过全量或增量流程导入。Sub2API 成功后，同一批变化账号会交给 Cockpit；两个目标都完成后才更新明文快照。日志和 manifest 只保存数量、账号标识和状态，不保存 token 内容。

## 增量同步

`incremental.bat` 固定读取与完整流程相同的输入。第一次运行建立安全基线，并只读查询当前带有本工具归属标记的 Sub2API 账号；普通已有账号不导入，但兑换结果明确提示“授权已更新”的已归属账号会在本轮导入。以后运行只导入新增账号和授权更新账号，未变化账号跳过。输入中减少的账号会输出日志并写入待手动处理清单，工具不会调用任何删除接口。两类待导入账号都按“Sub2API 后 Cockpit”的顺序导入。

Sub2API 导入成功的账号会在 `extra.account_pipeline_managed` 写入固定值 `account-pipeline-v1`。同邮箱但没有这个标记的中转账户默认视为手动账户，脚本会显示“跳过手动账户”，不会更新或写入增量快照。需要认领旧版导入账户时，可在本机 Sub2API 配置中临时设置 `claim_existing_accounts: true`；它只对本轮输入中精确匹配的账户生效，完成一次认领后应恢复为 `false`。兑换来源和 `accounts.txt` 来源都属于脚本主动导入范围；修改输入文件范围时会重新建立安全基线，不会据此删除旧账号。旧版没有归属标记的增量快照也只会触发安全基线。

把卡密行改成 `# PLUS-...` 后，它不再参与本轮输入；下一次增量运行会把对应的减少账号写入待手动处理清单。即使所有卡密都被注释，也会跳过兑换并继续记录输入变化；完全空白的卡密文件仍会报错。没有归属标记的账号也不会发送到 Cockpit 自动导入。

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
├─ state/incremental.json     增量快照（只保存账号标识和 Sub2API ID）
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

这些缓存不写入 Git 工作区。若需要换到其他目录，可以在命令行指定 `--runtime-dir D:\Sub2API-cache`，或在被忽略的 `config.json` 中设置 `runtime_dir`。`input\redeem-codes.txt` 和 `input\accounts.txt` 是用户输入源，不是自动生成的缓存，仓库已将它们加入忽略规则。

## Sub2API 配置

复制 `sub2api\config.example.json` 到本机配置位置后按实际环境修改。默认查找顺序是：`C:\ProgramData\Sub2API\account-pipeline\sub2api.json`、已有的 `C:\ProgramData\Sub2API\codex-account-import.json` 配置、模块内示例配置。配置中的 `runtime_env_path` 支持绝对路径和相对于配置文件的路径；环境文件提供 `ADMIN_EMAIL`、`ADMIN_PASSWORD`，不会写入日志。

## 输入边界和生成文件

主流程有两个独立外部输入：`input\redeem-codes.txt` 保存卡密，`input\accounts.txt` 保存已有账户 JSON 文本。账户文本中的每个对象必须是完整 JSON，但对象之间可以空行；支持单个对象、数组、JSONL 和连续对象。截图中常见的 `type: "oauth"` 且 `platform: "openai"` 会统一转换为 `type: "codex"`。

有卡密时先由 `redeem` 下载并解压；随后 `normalize` 同时读取解压目录和账户文本，按邮箱或账号 ID 去重，生成 `normalized\sub2api-accounts.json` 和 `normalized\cockpit-accounts.json`。同一账号在不同输入中完全相同可以合并；邮箱、`account_id` 或三个 token 字段不同时会覆盖写入 `cache\results\input-conflicts.txt` 并停止导入，报告只列账号、来源和不同字段，不包含 token 内容。日志会列出兑换来源、账户文本来源、重复数和合并后的总数；这两个 JSON 是各自导入器的单独重跑输入。标准化要求每个账号有 `access_token` 和可识别的邮箱。

兑换结果中有失效卡密时，已下载的账号和 `accounts.txt` 仍会继续合并导入，异常只写入结果和日志，不改变流程成功状态；输入中减少的账号仍只记录待手动处理清单，不会自动删除。这个情况的运行记录会标记为 `success_with_redeem_warnings`。
