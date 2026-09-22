# Redeem 模块

读取一行一个卡密的文本，提交兑换站任务，按阶段显示逐卡进度，并把最近一次结果覆盖写入固定的 `redeem-result.txt`。兑换站有时会在同一任务中把进度重新从 `1/N` 开始；程序会把回退识别为新阶段，明确标注“账号提取”“账号检测”或“后续检测”，不会再出现无法判断用途的重复进度。控制台结果按终端宽度对齐，长账号和说明会在续行显示；结果文件保留为 UTF-8 BOM 的 TSV，方便后续程序读取。成功下载的 ZIP 会保存到运行目录并安全解压，拒绝目录穿越、符号链接和超出大小限制的压缩包。

单独运行：

```bat
run.bat [卡密文件]
run.bat --action check [卡密文件]
```

未传文件时读取 `..\input\redeem-codes.txt`。结果和缓存默认写入 `C:\ProgramData\Sub2API\account-pipeline`；可通过 `--result-file`、`--cache-dir` 或 `--run-dir` 覆盖。
