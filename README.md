# Sub2ApiExt

Sub2API 的综合部署扩展工程。本仓库包含 `rate-sync`、`monitoring` 以及 Windows 综合一键部署入口；主 Sub2API 仍使用官方 Docker 镜像部署，本仓库不包含也不修改其上游源码。

## 服务

- `rate-sync`：同步账号倍率，并根据成功请求维护快慢成本记忆、动态调整分组倍率。
- `monitoring`：以真实请求和恢复探测判断账户状态，再按当前可路由账户聚合分组状态；仅在渠道报错后低频恢复探测，提供健康、延迟、历史、实时并发与 Tokens 用量面板。

两个扩展在编译时不依赖 Sub2API Go 源码。综合入口会先部署或升级主 Sub2API（含 PostgreSQL、Redis），再部署扩展；扩展运行时连接主服务的 Docker 网络、PostgreSQL 和 Admin API。升级主服务后仍需验证数据库结构及 API 兼容性。

## 一键部署

前置条件：

- Windows 10/11；
- Docker Desktop 和 Docker Compose；
- Docker Desktop 已安装并运行，且 `docker info`、`docker compose version` 可正常执行；首次部署不要求预先创建 Sub2API 容器。
- 主服务首次部署或检查升级需要访问 GitHub Releases/镜像源。

综合入口会先检查运行目录权限，必要时弹出 UAC 管理员确认。

部署主服务和全部扩展：

```bat
一键部署.bat
```

`deploy-all.bat` 是功能相同的英文文件名入口。首次运行会部署 Sub2API、PostgreSQL、Redis；已有部署再次运行时，会显示当前/目标版本，检测到新版本后询问 `y/N`，确认后先备份再升级主服务。随后每次都会从当前 Git 工程重新构建并更新两个扩展容器。

也可以分别运行：

```bat
rate-sync\deploy.bat
monitoring\deploy.bat
```

主服务运行文件、备份、日志及扩展运行文件会安装到：

```text
C:\ProgramData\Sub2API\
├── runtime\                         # Sub2API、PostgreSQL、Redis
├── backups\                         # 升级/回退备份
├── logs\                            # 部署日志
└── extensions\
    ├── rate-sync\
    └── monitoring\
```

部署完成后，主服务和扩展的 Docker Compose 工作目录、配置、状态卷和密钥都位于 `C:\ProgramData\Sub2API`，运行时不依赖旧工程目录；重新构建扩展新版本时仍需要本仓库源码。

如果检测到旧工程目录部署的 Sub2API，综合入口会在确认数据目录和挂载安全后迁移到 `runtime`；必须在旧工程目录仍存在时完成一次迁移并验证容器健康，之后才可以删除旧工程目录。旧目录删除后，脚本无法从已停止或不完整的旧栈恢复原配置。首次迁移也会优先复制现有 rate-sync 容器使用的配置。以后重复部署会保留 ProgramData 中的真实配置和状态卷。

主服务升级只替换 `sub2api` 应用镜像，不自动升级 PostgreSQL 或 Redis；升级失败会恢复升级前备份。单独运行 `rate-sync\deploy.bat` 或 `monitoring\deploy.bat` 仍只更新对应扩展，并要求主服务和 PostgreSQL 已经运行。

如果检测到原工作目录部署的 `sub2api-monitoring-standalone`，安装器会先等待新监控容器健康，再移除旧容器，避免两个 Worker 并行探测。

监控面板默认监听 `0.0.0.0:18090`，部署完成后可从同一局域网通过主机名或 IP 访问。部署脚本会校验或创建仅允许 Domain/Private 配置文件和本地子网的 Windows 防火墙规则，无法安全配置时会停止部署；如只需本机访问，可在安装后的 `monitoring\.env` 中把 `MONITORING_BIND_HOST` 改为 `127.0.0.1`，再重新运行部署脚本。

监控部署和根目录的一键部署都会自动运行本地访问修复脚本。脚本读取 Sub2API PostgreSQL 中的 `frontend_url` / `api_base_url`、Docker 实际端口、监控 `.env` 和 `settings.env`，合并 `MONITORING_FRAME_ANCESTORS`，校验局域网绑定/防火墙并在需要时重启监控。它不会调用 Sub2API Admin API，不会创建或修改 Sub2API 菜单，也不会回写网关数据库设置。

“渠道监控”菜单请你在 Sub2API 中手动配置。配置完成后，可随时单独运行：

```bat
monitoring\fix-monitoring-access.bat
```

两台机器的菜单 URL 分别填写实际可访问的监控地址：

```powershell
# 有域名的机器（建议 HTTPS）
# 在 Sub2API 自定义菜单中填写：https://monitor.example.com

# 没有域名的机器（使用局域网 IP）
# 在 Sub2API 自定义菜单中填写：http://192.168.1.20:18090
```

无域名机器建议做 DHCP 保留或设置静态 IP；如果 IP 变更，重新运行 `monitoring\fix-monitoring-access.bat`，并同步修改 Sub2API 菜单 URL。也可以使用公司内网 DNS/稳定主机名替代 IP。

如果通过别名打开 Sub2API，而该别名没有写在数据库的 `frontend_url` / `api_base_url` 中，请把该来源追加到 `C:\ProgramData\Sub2API\extensions\monitoring\settings.env` 的 `MONITORING_FRAME_ANCESTORS`，然后运行修复脚本。不要使用 `*`；父页面使用 HTTPS 时，菜单中的监控地址也必须使用 HTTPS。

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
