# Sub2API 模块

使用 Python 调用 Sub2API Admin API，完成管理员登录、分组和代理校验、账号幂等导入及变更字段显示。只允许配置的本机回环地址，错误信息不会输出响应正文或凭据。

单独运行：

```bat
run.bat <sub2api-accounts.json> --config <配置文件>
import-from-cockpit-tools.bat [--what-if]
```

`import-from-cockpit-tools.bat` 会读取当前 Windows 用户的 Cockpit Tools 加密账号，需要安装 `requirements.txt` 中的 `cryptography` 包。
