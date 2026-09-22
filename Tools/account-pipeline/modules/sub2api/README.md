# Sub2API 模块

这是现有 `import-codex-accounts.ps1` 的 Python 启动适配器。它沿用原有配置、管理员登录、分组、代理和幂等更新逻辑，不直接访问数据库。

单独运行示例：`python main.py <sub2api-accounts.json> --config <配置文件>`。
