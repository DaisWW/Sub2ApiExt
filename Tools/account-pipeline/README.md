# 账号流水线

总入口 `run.bat` 启动 Python 编排器。完整流程支持两种输入：一行一个卡密的 TXT，以及可连续放置多个账户 JSON 对象的账户文本。两个输入同时存在时会合并、去重后生成统一数据，再分别导入 Sub2API 和 Cockpit。编排器依次调用四个独立模块：

1. `redeem`：提交卡密，按提取/检测阶段显示逐卡结果，覆盖写入固定结果 TXT，下载 ZIP 并安全解压。
2. `normalize`：读取解压目录中的账号 JSON 和可选账户文本，去重并生成两个目标输入文件。
3. `sub2api`：读取标准化的 `sub2api-accounts.json`，调用 Sub2API Admin API，按账号执行幂等导入并显示变更字段。
4. `cockpit`：通过回环一次性 HTTP 链接把标准化 JSON 交给 Cockpit Tools。

## 使用

```bat
run.bat
run.bat D:\input\redeem-codes.txt --skip-cockpit
run.bat --codes-file D:\input\redeem-codes.txt --accounts-file D:\input\accounts.txt
run.bat D:\input\accounts.txt --skip-sub2api --skip-cockpit
run.bat --dry-run
```

不传任何输入参数时分别读取 `input\redeem-codes.txt` 和 `input\accounts.txt`（存在才读取），适合一键同时处理两种来源。卡密文件一行一个卡密，空行和 `#` 开头的行会忽略。账户文本可以是单个对象、数组、JSONL，或多个完整 JSON 对象连续放置，中间允许空行；不要求整个文件再包一层数组。位置参数会自动识别卡密或账户文本，并只处理拖入的这一种来源；使用任一 `--codes-file`、`--accounts-file` 时只读取明确指定的来源，两个选项一起传才会合并外部文件。`--dry-run` 只验证本地输入，不访问兑换站或导入服务。

首次使用可复制 `input\accounts.example.txt` 为 `input\accounts.txt`，再把真实账户对象替换进去；示例只使用占位令牌。

各模块也能单独运行：

```bat
redeem\run.bat [卡密文件]
normalize\run.bat --data-dir <解压目录> --output-dir <输出目录>
normalize\run.bat --input-file <账户文本> --output-dir <输出目录>
sub2api\run.bat <sub2api-accounts.json> --config <配置文件>
sub2api\import-from-cockpit-tools.bat [--what-if]
cockpit\run.bat <cockpit-accounts.json>
```

根目录的 `run.bat` 是唯一的 Python 启动实现；模块 BAT 只传入目标脚本和参数，因此根目录不会再有第二个公共启动 BAT。

旧快捷入口对应的新位置是：`redeem\redeem-account-import.bat`、`cockpit\drop-json-to-import.bat` 和 `sub2api\import-from-cockpit-tools.bat`。最后一个只读取 Cockpit Tools 本地账号，是 Sub2API 的备选来源，不参与总流程。BAT 只负责找到 Python、传递参数和显示退出码，业务逻辑全部在 `.py` 文件中。

## 运行目录

默认运行目录是工具目录下被 Git 忽略的 `cache`，即 `Tools\account-pipeline\cache`，可在本目录创建被 gitignore 的 `config.json` 覆盖：

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
│  ├─ redeem-manifest.json    兑换元数据
│  └─ manifest.json           阶段状态和退出码
├─ logs/pipeline-*.log        流水线日志
└─ results/redeem-result.txt  最近一次逐卡结果（UTF-8 BOM TSV）
```

manifest 只保存路径、数量、任务号和阶段状态，不保存卡密、Token、密码或账号凭据。`normalized` 下的 JSON 含凭据，只保存在本机运行目录。

缓存和过渡文件的实际位置如下：

| 内容 | 默认位置 |
| --- | --- |
| 下载的 ZIP | `Tools\account-pipeline\cache\runs\<run-id>\redeem\` |
| ZIP 解压出的账号 JSON | `Tools\account-pipeline\cache\runs\<run-id>\redeem\data\` |
| 给 Sub2API 和 Cockpit 的标准化 JSON | `Tools\account-pipeline\cache\runs\<run-id>\normalized\` |
| 本次运行 manifest | `Tools\account-pipeline\cache\runs\<run-id>\manifest.json` |
| 最近一次卡密结果 | `Tools\account-pipeline\cache\results\redeem-result.txt` |
| 流水线日志 | `Tools\account-pipeline\cache\logs\` |

这些缓存不写入 Git 工作区。若需要换到其他目录，可以在命令行指定 `--runtime-dir D:\Sub2API-cache`，或在被忽略的 `config.json` 中设置 `runtime_dir`。`input\redeem-codes.txt` 和 `input\accounts.txt` 是用户输入源，不是自动生成的缓存，仓库已将它们加入忽略规则。

## Sub2API 配置

复制 `sub2api\config.example.json` 到本机配置位置后按实际环境修改。默认查找顺序是：`C:\ProgramData\Sub2API\account-pipeline\sub2api.json`、已有的 `C:\ProgramData\Sub2API\codex-account-import.json` 配置、模块内示例配置。配置中的 `runtime_env_path` 支持绝对路径和相对于配置文件的路径；环境文件提供 `ADMIN_EMAIL`、`ADMIN_PASSWORD`，不会写入日志。

## 输入边界和生成文件

主流程有两个独立外部输入：`input\redeem-codes.txt` 保存卡密，`input\accounts.txt` 保存已有账户 JSON 文本。账户文本中的每个对象必须是完整 JSON，但对象之间可以空行；支持单个对象、数组、JSONL 和连续对象。截图中常见的 `type: "oauth"` 且 `platform: "openai"` 会统一转换为 `type: "codex"`。

有卡密时先由 `redeem` 下载并解压；随后 `normalize` 同时读取解压目录和账户文本，按账号 ID 或邮箱去重，生成 `normalized\sub2api-accounts.json` 和 `normalized\cockpit-accounts.json`。这两个 JSON 是各自导入器的单独重跑输入。标准化要求每个账号有 `access_token` 和可识别的邮箱。
