# Redeem 模块

这是现有兑换器的薄适配层。它接收卡密文件和本次运行目录，调用 `codex-account-import/redeem-account-import.py`，因此兑换站协议只维护一份。

结果固定由总流程写入 `results/redeem-result.txt`；ZIP 和解压数据写入本次运行目录。退出码 `2` 表示有部分卡密失败，但只要仍有可用下载数据，总流程会继续标准化；后续导入完成后总流程仍返回 `2`，方便启动器发现部分失败。

单独运行示例：`python main.py <卡密文件> --run-dir <运行目录> --result-file <结果文件> --manifest-file <manifest>`。
