# Normalize 模块

读取兑换模块解压出的 `.json`/`.jsonl` 文件，识别常见的 Sub2API `accounts[].credentials`、数组和单账号结构，提取邮箱与 Codex 凭据并去重。

记录显式填写 `type` 时必须是 `codex`；缺少 `type` 的旧版账号 JSON 会补成 `codex`，其他类型会停止处理。

输出：

- `sub2api-accounts.json`：Sub2API 数据包格式；
- `cockpit-accounts.json`：Cockpit 导入数组；
- `manifest.json`：文件数量和账号数量等非敏感元数据。

单独运行示例：`python main.py --data-dir <解压目录> --output-dir <输出目录>`。
