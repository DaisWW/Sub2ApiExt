# 卡密提取缓存工具

双击 `redeem-account-import.bat` 时，脚本读取同目录的 `redeem-codes.txt`（可先复制 `redeem-codes.example.txt`）；也可以把一个“一行一个卡密”的文本文件拖到 BAT 上。默认操作是下载 Sub2API 格式。

每次任务会：

1. 访问 `https://redeem.plusproteam.xyz/` 建立会话并提交卡密。
2. 等待任务完成，显示网页返回的逐卡结果。
3. 把压缩包保存到 `cache/run-时间-任务号/`，再解压到该目录的 `data/`。
4. 用 UTF-8（带 BOM）的制表符文本覆盖 `redeem-result.txt`。

结果文件的列固定为：`卡密`、`账号`、`首次提取时间`、`剩余质保期`、`结果`、`说明`。这样既与网页表格一致，也能由后续脚本按 TSV 读取。

需要只检测而不下载时，可在工具目录运行：

```powershell
python.exe .\redeem-account-import.py --action check
```

卡密文件、缓存和结果文件已加入 Git 忽略规则；提交时卡密会发送到上述兑换站，下载的账号 JSON 只写入本机缓存目录，不会写入 Git。
