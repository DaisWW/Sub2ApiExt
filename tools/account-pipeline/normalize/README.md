# Normalize 模块

读取兑换包解压目录中的 `.json`/`.jsonl`，或读取一个可含多个连续 JSON 对象的账户文本；识别常见的单账号、数组和 `accounts[].credentials` 结构，提取 Codex 凭据并按邮箱去重。对象之间可以空行，不要求整个文本是一个完整数组。

同一邮箱或同一 `account_id` 的重复记录只有在邮箱、账号 ID 和三个 token 字段一致时才会合并。任一字段不同时停止生成导入文件，并写出只含账号、来源和字段名的 `input-conflicts.txt`，不会把 token 内容写入冲突报告。

显式填写 `type` 时支持 `codex` 和来自 Cockpit 的 `oauth`；`oauth` 记录的平台若填写，只接受 `openai` 或 `codex`，输出时统一为 `codex`。缺少 `type` 的旧格式会补成 `codex`，其他类型会停止处理。输出 `sub2api-accounts.json`、`cockpit-accounts.json` 和不含凭据的 `manifest.json`。

单独运行：

```bat
run.bat --data-dir <解压目录> --output-dir <输出目录>
run.bat --input-file <账户文本> --output-dir <输出目录>
run.bat --data-dir <解压目录> --input-file <账户文本> --output-dir <输出目录>
```
