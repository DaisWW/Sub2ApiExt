# Cockpit 模块

读取标准化的 `cockpit-accounts.json`，启动本机 Cockpit Tools，并用一次性回环 HTTP 请求交付 JSON。临时端口、随机路径和等待时间只在当前运行中有效。

单独运行：

```bat
run.bat <cockpit-accounts.json>
```

未传文件时会打开文件选择器。此模块只支持 Windows 和已安装的 Cockpit Tools。
