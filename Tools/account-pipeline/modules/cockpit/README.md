# Cockpit 模块

Python 适配器调用本目录的 `bridge.ps1`。bridge 使用回环 HTTP 一次性交付标准化的账号 JSON，并让 Cockpit Tools 自己完成导入。原有 `Tools/cockpit-tools-import` 文件保持不动。

单独运行示例：`python main.py <cockpit-accounts.json>`。
