# Sub2API 模块

使用 Python 读取标准化的 `sub2api-accounts.json`，调用 Sub2API Admin API，完成管理员登录、分组和代理校验、账号幂等导入及变更字段显示。总流程中的来源是 Redeem 生成的 JSON；Cockpit 本地账号读取只保留为备选入口。只允许配置的本机回环地址，错误信息不会输出响应正文或凭据。

单独运行：

```bat
run.bat <sub2api-accounts.json> --config <配置文件>
import-from-cockpit-tools.bat [--what-if]
```

`run.bat` 用于导入 JSON；`import-from-cockpit-tools.bat` 才会读取当前 Windows 用户的 Cockpit Tools 加密账号，需要安装 `requirements.txt` 中的 `cryptography` 包。后者不会被总流程自动调用。
