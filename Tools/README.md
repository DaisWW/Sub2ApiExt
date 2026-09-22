# Tools 工具目录

`account-pipeline` 是账号处理的唯一总目录，四个子目录各自只负责一个模块：

```text
Tools/
├─ README.md
└─ account-pipeline/
   ├─ run.bat                         一键：兑换 → 解压 → 标准化 → 双目标导入
   ├─ run.py                          Python 流程编排
   ├─ run-python.bat                  所有快捷入口共用的 Python 启动器
   ├─ input/                          卡密输入（本地文件被 gitignore）
   ├─ redeem/                         兑换、结果文件、下载和安全解压
   ├─ normalize/                      账号 JSON 标准化
   ├─ sub2api/                        Sub2API Admin API 导入
   └─ cockpit/                        Cockpit Tools 本地导入
```

把卡密文件拖到 `account-pipeline\run.bat`，或把卡密逐行写入 `input\redeem-codes.txt` 后双击。结果会显示在窗口，并覆盖保存到 `C:\ProgramData\Sub2API\account-pipeline\results\redeem-result.txt`。

缓存、解压目录、标准化 JSON 和运行 manifest 存放在 `C:\ProgramData\Sub2API\account-pipeline\runs`，不写入 Git 工作区。完整参数和单模块用法见 [account-pipeline/README.md](account-pipeline/README.md)。
