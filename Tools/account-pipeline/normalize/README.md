# Normalize 模块

读取兑换包解压目录中的 `.json`/`.jsonl`，识别常见的单账号、数组和 `accounts[].credentials` 结构，提取 Codex 凭据并按账号 ID 或邮箱去重。

显式填写 `type` 时必须是 `codex`；缺少 `type` 的旧格式会补成 `codex`，其他类型会停止处理。输出 `sub2api-accounts.json`、`cockpit-accounts.json` 和不含凭据的 `manifest.json`。

单独运行：

```bat
run.bat --data-dir <解压目录> --output-dir <输出目录>
```
