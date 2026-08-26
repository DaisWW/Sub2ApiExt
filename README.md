# Sub2ApiExt

Sub2API 的独立扩展服务。本仓库不包含也不修改 Sub2API 上游源码。

## 服务

- `rate-sync`：同步账号倍率，并根据成功请求维护快慢成本记忆、动态调整分组倍率。
- `monitoring`：探测账号和分组状态，提供健康、延迟、历史与 Tokens 用量面板。

两个服务在编译时不依赖 Sub2API Go 源码。运行时会连接已经部署的 Sub2API Docker 网络、PostgreSQL 和 Admin API，因此升级 Sub2API 后仍需验证数据库结构及 API 兼容性。

## 一键部署

前置条件：

- Windows 10/11；
- Docker Desktop 和 Docker Compose；
- 官方 Sub2API 容器已经运行，容器名为 `sub2api` 和 `sub2api-postgres`。

部署全部扩展：

```bat
一键部署.bat
```

`deploy-all.bat` 是功能相同的英文文件名入口。每次执行都会从当前 Git 工程重新构建镜像并更新扩展容器。

也可以分别运行：

```bat
rate-sync\deploy.bat
monitoring\deploy.bat
```

安装器只检查 Sub2API 容器并读取数据库连接信息，不会重建或更新 Sub2API、PostgreSQL、Redis。扩展镜像构建完成后，最小运行文件会安装到：

```text
C:\ProgramData\Sub2API\extensions\
├── rate-sync\
└── monitoring\
```

扩展容器的 Compose 工作目录、配置和密钥都位于该目录。部署完成后，Docker 启动和容器重启不再依赖本 Git 工作区。重新构建扩展新版本时仍需要本仓库源码，或使用以后发布到镜像仓库的预构建镜像。

首次迁移会优先复制现有 rate-sync 容器使用的配置。以后从本仓库重复部署会保留 ProgramData 中的真实配置和 Docker 状态卷。

监控面板默认仅监听 `127.0.0.1:18090`。可在安装后的 `monitoring\.env` 中调整监听地址和端口，再从该目录执行 `docker compose up -d`。

每个运行目录都会安装 `manage.bat`：

```bat
manage.bat status
manage.bat logs
manage.bat restart
manage.bat stop
manage.bat start
```

## 配置

Git 只保存配置模板：

- `rate-sync/config.example.json`
- `rate-sync/account-config.example.json`
- `monitoring/settings.env.example`

真实数据库密码由部署脚本从现有 PostgreSQL 容器读取，并写入 ProgramData 下受限权限的运行文件，不会写入本仓库。

## 开发验证

```powershell
Set-Location rate-sync
go test ./...

Set-Location ..\monitoring
go test ./...
```

## License

Apache-2.0
