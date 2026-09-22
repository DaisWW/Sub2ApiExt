# Tools 工具目录

各子目录是可以单独运行的模块；需要完整处理时使用 `account-pipeline` 总入口。

```text
Tools/
├─ account-pipeline/                 总流程：卡密 → JSON → 两个导入目标
│  ├─ run.bat                         总流程启动 BAT
│  ├─ run.py                          Python 编排器
│  ├─ input/                          本地卡密输入（gitignored）
│  └─ modules/
│     ├─ redeem/                      兑换适配器
│     ├─ normalize/                   JSON 标准化
│     ├─ sub2api/                     Sub2API 适配器
│     └─ cockpit/                     Cockpit 适配器和 PowerShell bridge
├─ codex-account-import/              原有 Sub2API 导入器和兼容旧入口
└─ cockpit-tools-import/              原有 Cockpit 单独导入入口
```

日常使用：把卡密文本拖到 `Tools\account-pipeline\run.bat`，或在 `account-pipeline\input\redeem-codes.txt` 填好后双击。完整目录说明、缓存位置和中间文件格式见 [account-pipeline/README.md](account-pipeline/README.md)。

旧目录不会被总流程删除或覆盖，方便已有脚本继续使用；新流程只把用户已有 Cockpit bridge 复制到自己的模块目录中维护。
