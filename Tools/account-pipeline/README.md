# 账号流水线

这个目录是统一入口。`run.bat` 只负责启动 Python；Python 按顺序调用四个独立模块：

1. `modules/redeem`：复用 `Tools/codex-account-import/redeem-account-import.py`，提交卡密、显示逐卡结果、下载 ZIP 并安全解压。
2. `modules/normalize`：从解压目录读取 JSON，去重并生成两个目标文件。
3. `modules/sub2api`：调用现有 `import-codex-accounts.ps1`，不重复实现 Sub2API Admin API。
4. `modules/cockpit`：调用 `bridge.ps1`，通过 Cockpit Tools 的本地导入协议交付 JSON。

## 使用

- 把一行一个卡密的文本文件拖到 `run.bat`；
- 或把卡密写入 `input\redeem-codes.txt` 后双击 `run.bat`；
- 旧位置 `Tools\codex-account-import\redeem-codes.txt` 仍然兼容；
- `--dry-run` 只检查本地文件，不访问兑换站；
- `--skip-sub2api` 或 `--skip-cockpit` 可在需要时跳过一个导入目标。

如果部分卡密失败但仍有成功账号，后续两个导入仍会执行，最终退出码为 `2`；`manifest.json` 的 `status` 会是 `partial`。

示例：

```powershell
.\run.bat --dry-run
.\run.bat D:\input\codes.txt --skip-cockpit
```

Sub2API 的导入配置仍由原有脚本管理：默认读取 `C:\ProgramData\Sub2API\codex-account-import.json`，不存在时使用 `Tools\codex-account-import\config.example.json`。也可以在这里创建 gitignored 的 `config.json`（其中相对 `sub2api_config` 路径以本目录为基准），或用 `--sub2api-config` 指定配置。

## 运行目录

默认运行数据不会写回 Git 工作区：

```text
C:\ProgramData\Sub2API\account-pipeline\
├─ runs\<run-id>\
│  ├─ redeem\              下载的 ZIP 和 data\
│  ├─ normalized\          sub2api-accounts.json、cockpit-accounts.json
│  ├─ redeem-manifest.json 非敏感兑换元数据
│  └─ manifest.json        本次流水线状态和退出码
├─ results\redeem-result.txt  固定覆盖的逐卡结果（UTF-8 BOM TSV）
└─ logs\                    预留给外部启动器
```

标准化文件包含账号凭据，是后续导入的中间数据，只保存在本机运行目录，不进入 Git。manifest 只记录路径、数量、任务号和阶段状态，不写入卡密或 Token。

结果文本固定为六列：`卡密`、`账号`、`首次提取时间`、`剩余质保期`、`结果`、`说明`；每次运行覆盖上一次内容。

## 生成的两个输入格式

`normalized\sub2api-accounts.json` 使用 Sub2API 数据包形态：

```json
{
  "type": "sub2api-data",
  "version": 1,
  "accounts": [{"name": "email@example.com", "credentials": {"type": "codex"}}]
}
```

`normalized\cockpit-accounts.json` 是 Cockpit 导入桥接所需的账号数组。标准化会从账号字段或 JWT payload 提取邮箱；若无法得到邮箱就停止，避免生成无法同时导入两个目标的半成品。
