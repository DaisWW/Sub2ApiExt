# tools 工具目录

`account-pipeline` 是账号处理的唯一总目录，四个子目录各自只负责一个模块：

```text
tools/
├─ README.md
└─ account-pipeline/
   ├─ run.bat                         一键：读取两种输入 → 标准化 → 双目标导入
   ├─ run.py                          Python 流程编排
   ├─ input/                          卡密和账户文本输入（本地文件被 gitignore）
   ├─ redeem/                         兑换、结果文件、下载和安全解压
   ├─ normalize/                      账号 JSON 标准化
   ├─ sub2api/                        Sub2API Admin API 导入
   └─ cockpit/                        Cockpit Tools 本地导入
```

把卡密文件拖到 `account-pipeline\run.bat`，或把卡密逐行写入 `input\redeem-codes.txt`；需要直接导入已有账户时，把多个 JSON 对象（对象之间可空行）写入 `input\accounts.txt`。两个文件同时存在时，一键流程会合并账号、整理格式，然后按 Sub2API → Cockpit 的顺序全量导入。结果会显示在窗口，并覆盖保存到 `account-pipeline\cache\results\redeem-result.txt`。需要只处理新增账号、授权已更新账号并记录输入中减少账号时使用 `account-pipeline\incremental.bat`。

根目录只保留一个总入口 BAT；各模块目录中的 `run.bat` 是对应模块的快捷入口，会复用总入口的 Python 启动检查。

下载缓存、解压目录、标准化 JSON、增量快照、日志和运行 manifest 存放在被 Git 忽略的 `account-pipeline\cache`，不写入 Git 工作区。完整路径、清理方式和单模块用法见 [account-pipeline/README.md](account-pipeline/README.md)。
