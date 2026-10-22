# 部署交接

## 当前部署形态
- 服务器从本仓库源码用 Docker Compose 构建，容器端口只绑定服务器本机：API `127.0.0.1:18080`，管理端 `127.0.0.1:18081`。
- 固定启用管理模式（`KIRO_ADMIN_DATA_DIR=/app/data`），空账号可启动；`./data` 需归 UID/GID 1001。
- `env_file: .env` 把 `.env` 全部变量传入容器；端口、监听地址、数据目录由 Compose 固定。
- Nginx：`9002` 对外转发 API；`127.0.0.1:9003` 转发管理端并注入 `X-Kiro-Admin-Token`，用户通过 SSH 隧道（本地 `19003`）访问，`KIRO_ADMIN_ORIGINS=http://127.0.0.1:19003`。
- 完整步骤见 `docs/deployment.md`。

## Git 状态
- 已推送：`eea52a1`（Compose 改为 `env_file` 传递 `.env`）。
- 本次：重写 `docs/deployment.md` 为完整部署文档（Nginx、SSH 隧道、NewAPI、运维、常见问题），同步 README 部署段落与本文件。仅文档，未改源码和配置。提交、推送需用户另行授权。

## 已验证 / 未验证
- 用户在服务器上：容器已能启动，管理后台经 Nginx 注入 + SSH 隧道可进入，来源对齐后可添加账号。
- Pi 本机：只做过 Compose 渲染检查，未构建镜像、未运行容器。
- 未确认：启用 `KIRO_LOCAL_CACHE_DISCOUNT=true` 后 NewAPI 是否显示缓存 token，待用户在服务器实测。

## 续接
说“继续 kiro2api 部署”，先读本文件和 `docs/deployment.md`，检查 Git 状态。用户自行在服务器操作，不擅自 SSH；commit、push 分别授权。
