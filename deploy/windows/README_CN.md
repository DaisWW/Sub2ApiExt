# Windows 综合一键部署

运行前请手动安装并启动 Docker Desktop，确认 `docker info` 和 `docker compose version` 可正常执行。

> 迁移提示（重要）：如果机器上仍运行从旧 `sub2api-main` 目录启动的主栈，请在旧目录仍存在时先运行当前工程的 `一键部署.bat`，确认 `C:\ProgramData\Sub2API\runtime` 下的配置、三个数据目录和容器健康状态都已就绪，再删除旧目录。旧目录删除后，脚本无法从已停止或不完整的旧栈恢复原配置；删除旧目录不是迁移步骤。

## 使用方式

在仓库根目录双击：

```text
一键部署.bat
```

用户只需要运行这一个入口。请保留当前 Git 工程中的完整部署目录：

```text
一键部署.bat
deploy-all.bat
deploy-all.ps1
deploy\windows\
rate-sync\
monitoring\
scripts\
```

入口会先以管理员权限运行主服务部署管理器，再部署两个扩展；主服务和扩展均来自当前 Git 工程中的脚本，不依赖已经删除的旧工程目录。

首次全新部署完成后：

```text
访问地址：http://本机IP:18080
管理员：admin@sub2api.local
初始密码：admin123456
```

初始密码只在空数据库首次初始化时写入，登录后请立即修改。

## 自动流程

- 只检查 Docker CLI、Docker Compose 和 Docker 引擎状态
- 不安装、启动或升级 Docker，不启用 WSL，不修改 Windows 服务和计划任务
- 第一次运行直接部署 GitHub 最新正式版，不显示升级询问
- 已部署时显示 Docker 版本、Sub2API 当前版本和目标版本
- 检测到新版本时询问 `y/N`；确认后先创建完整备份，再升级 Sub2API 应用容器
- 主服务升级不替换 PostgreSQL 或 Redis；两个扩展每次重新构建并启动
- 部署与升级直接使用正式版 Docker 镜像，不要求安装 Git，也不需要 `git pull`
- Windows 部署优先使用官方镜像源；Sub2API、PostgreSQL 和 Redis 下载失败时自动使用 DaoCloud 加速地址；发现旧版目录型 Compose 部署时会保留原目录并安全接管
- 升级前保存应用镜像以及 PostgreSQL、Redis、配置和应用数据的一致性快照
- 升级失败时自动恢复升级前备份
- 同一个入口支持选择历史备份进行回退；回退前会再次备份当前状态

主服务管理器完成后，综合入口会继续部署 `rate-sync` 和 `monitoring`。单独运行这两个目录下的 `deploy.bat` 仍只更新对应扩展，并要求主服务和 PostgreSQL 已经运行。

## 数据位置

```text
C:\ProgramData\Sub2API\
├── runtime\     # .env、Compose 和全部持久化数据
├── backups\     # 升级及回退备份，不自动删除
└── logs\        # 管理器日志
```

升级和回退不会执行 `docker compose down -v`，也不会删除 Docker 数据卷。备份使用停止容器后的物理数据快照，以便数据库迁移后仍能完整回退。

仅删除容器或重装 Docker 不会重置端口和管理员密码，因为运行配置与数据仍保留在 `C:\ProgramData\Sub2API\runtime\`。需要全新部署时，请先停止容器并将 `C:\ProgramData\Sub2API\` 重命名为备份目录，再重新运行一键部署。

每次运行的完整输出保存在 `C:\ProgramData\Sub2API\logs\manager-时间.log`。提权或管理器启动失败时，另见同目录下的 `bootstrap.log` 和兼容日志 `manager.log`。

容器使用 `restart: unless-stopped`。需要开机自动运行时，请在 Docker Desktop 中自行启用登录后启动。
