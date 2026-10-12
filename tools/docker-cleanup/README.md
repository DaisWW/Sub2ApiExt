# Docker 清理

适用于本工程部署后的旧 Sub2API 镜像和 Docker 构建缓存。Docker Desktop 必须已经运行；脚本读取 `C:\ProgramData\Sub2API` 中的配置和备份清单。

## 保留策略

- 保留最近 **14 天**创建的镜像和自动保存的回滚镜像；只有超过保留期且未受保护的 Sub2API 镜像标签才会删除。
- 每个扩展额外保留最近 **2 个不同的旧版本**，即使超过 14 天也保留。自动回滚标签按保存时间排序，历史手工回滚标签按镜像创建时间排序；自动标签的保存机制见 [Windows 部署说明](../../deploy/windows/README_CN.md)。
- 所有现有容器（包括停止的容器）引用的镜像、配置中的主服务镜像、部署用的 `local` 标签和正式备份引用的镜像始终受保护。
- 主服务的完整部署备份仍由安装器保留最近 **5 份**，本工具只读取备份清单。

14 天提供排查和回退窗口；2 个旧版本足够应对连续两次升级的问题。近期镜像可能多于 2 份，这个数量是回滚保留的下限。

## 独立使用

双击 `run.bat` 默认只预览；也可在仓库根目录执行：

```powershell
.\tools\docker-cleanup\run.bat
.\tools\docker-cleanup\run.bat -Apply -PruneBuildCache
```

`-Apply` 才会删除镜像。`-PruneBuildCache` 还清理当前 Docker 引擎中超过 14 天的悬空镜像，以及当前 Buildx builder 中超过 14 天未使用的构建缓存；下次构建可能需要重新生成缓存。`-IncludeRecent` 用于首次集中清理，会取消 Sub2API 镜像的 14 天门槛，但仍保留受保护镜像和最近两个回滚版本。

工具不会删除容器、数据卷、配置、状态或备份文件；配置、备份清单或引用的镜像缺失时停止清理。配置和备份镜像先解析为镜像 ID，支持标签、镜像 ID 和 digest；悬空镜像清理也遵守这些保护规则。单独运行 PowerShell 脚本同样默认只预览：

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File tools\docker-cleanup\cleanup-docker.ps1
```
