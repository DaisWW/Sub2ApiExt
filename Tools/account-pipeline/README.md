# 账号流水线

总入口 `run.bat` 只启动 Python 编排器。编排器依次调用四个独立模块：

1. `redeem`：提交卡密，显示逐卡结果，覆盖写入固定结果 TXT，下载 ZIP 并安全解压。
2. `normalize`：读取解压目录中的账号 JSON，去重并生成两个目标输入文件。
3. `sub2api`：调用 Sub2API Admin API，按账号执行幂等导入并显示变更字段。
4. `cockpit`：通过回环一次性 HTTP 链接把标准化 JSON 交给 Cockpit Tools。

## 使用

```bat
run.bat
run.bat D:\input\redeem-codes.txt --skip-cockpit
run.bat --dry-run
```

不传文件时读取 `input\redeem-codes.txt`；文件一行一个卡密，空行和 `#` 开头的行会忽略。拖入文件时只使用拖入文件。`--dry-run` 只验证本地布局，不访问兑换站或导入服务。

各模块也能单独运行：

```bat
redeem\run.bat [卡密文件]
normalize\run.bat --data-dir <解压目录> --output-dir <输出目录>
sub2api\run.bat <sub2api-accounts.json> --config <配置文件>
sub2api\import-from-cockpit-tools.bat [--what-if]
cockpit\run.bat <cockpit-accounts.json>
```

旧快捷入口对应的新位置是：`redeem\redeem-account-import.bat`、`cockpit\drop-json-to-import.bat` 和 `sub2api\import-from-cockpit-tools.bat`。BAT 只负责找到 Python、传递参数和显示退出码，业务逻辑全部在 `.py` 文件中。

## 运行目录

默认运行目录是 `C:\ProgramData\Sub2API\account-pipeline`，可在本目录创建被 gitignore 的 `config.json` 覆盖：

```json
{
  "runtime_dir": "C:\\ProgramData\\Sub2API\\account-pipeline",
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
└─ results/redeem-result.txt  最近一次逐卡结果（UTF-8 BOM TSV）
```

manifest 只保存路径、数量、任务号和阶段状态，不保存卡密、Token、密码或账号凭据。`normalized` 下的 JSON 含凭据，只保存在本机运行目录。

## Sub2API 配置

复制 `sub2api\config.example.json` 到本机配置位置后按实际环境修改。默认查找顺序是：`C:\ProgramData\Sub2API\account-pipeline\sub2api.json`、已有的 `C:\ProgramData\Sub2API\codex-account-import.json` 配置、模块内示例配置。配置中的 `runtime_env_path` 支持绝对路径和相对于配置文件的路径；环境文件提供 `ADMIN_EMAIL`、`ADMIN_PASSWORD`，不会写入日志。

## 生成文件

`normalized\sub2api-accounts.json` 是 Sub2API 数据包格式；`normalized\cockpit-accounts.json` 是 Cockpit 导入数组。标准化要求每个账号有 `access_token` 和可识别的邮箱，并拒绝显式声明为非 `codex` 的记录。
