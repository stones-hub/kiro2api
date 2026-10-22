# 部署配置交接

## 本次范围（Compose 传递 .env 全部配置）
修复服务器 `.env` 中的可选变量不进容器的问题：原 `docker-compose.yml` 只用 `environment` 白名单透传，`KIRO_LOCAL_CACHE_*`、`KIRO_STICKY_WAIT_SECONDS`、`KIRO_MODELS_FILE` 都收不到，导致本地缓存折算关闭、NewAPI 看不到缓存 token。

改动文件（仅部署配置与文档，未改 Go/JS 源码）：
- `docker-compose.yml`：新增 `env_file: - .env`；`environment` 原有固定值和必填密钥校验、端口映射、挂载、健康检查全部保持原样。
- `.env.example`：新增"本地缓存折算（可选）"与"账号粘滞（可选）"两段（全部注释掉），说明取值范围、命中条件（5 分钟内 + 同一 API Key/`metadata.user_id` + 请求前缀重复，首次请求不显示缓存）、这是本地计费折算而非上游真实缓存计量，以及空值会导致启动失败；修正 `KIRO_ADMIN_ORIGINS` 注释为"必须与浏览器地址栏来源完全一致（协议+主机+端口）"。
- `docs/deployment.md`：说明 `.env` 全量传入但端口/监听地址/数据目录由 Compose 固定覆盖；缓存配置简述；改 `.env` 后需 `docker compose up -d --force-recreate kiro2api`（`restart` 不更新环境变量）；核对变量名的 `docker exec ... env | cut -d= -f1 | grep -E ...` 命令；修正 SSH 隧道端口与 `KIRO_ADMIN_ORIGINS` 的匹配说明。

未改 `README.md`：其中没有这些环境变量的相关说明。

关键原理：这些变量在 `server/cache_discount.go` 和 `main.go` 中用 `LookupEnv` 读取，"存在但为空"会解析失败并 panic/退出，所以不能改成 `${VAR:-}` 形式逐个透传，只能用 `env_file` 整体传入；`.env` 里要么不写该行，要么写合法值。

## 本次状态
- Git 基线：d64c93e849340f9468f358c005988502138c856b；开始前工作区仅有未跟踪的任务书 `.pi/tasks/compose-env-file.md`。
- 用户已授权修改上述文件；**未**授权 commit/push/部署，本次改动均未提交未推送。
- 执行器：Claude Code，请求模型 opus（sonnet 首次调用报模型不可用，用户选择换模型重试）。
- 任务书：`.pi/tasks/compose-env-file.md`（未跟踪）。

## 本次验证
- 在 `/tmp` 临时目录下用合成密钥的 `.env`（未在仓库创建 `.env`，未读取真实凭据）执行 `docker compose --project-directory ... config`：`KIRO_LOCAL_CACHE_DISCOUNT`、`KIRO_LOCAL_CACHE_MAX_PERCENT`、`KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT`、`KIRO_LOCAL_CACHE_MIN_UNCACHED_TOKENS`、`KIRO_STICKY_WAIT_SECONDS`、`KIRO_MODELS_FILE` 均出现在渲染结果中。
- 同一份 `.env` 里写 `PORT=9999`、`KIRO_ADMIN_DATA_DIR=/x`、`BIND_HOST=1.2.3.4`、`KIRO_ADMIN_BIND=1.2.3.4:9998`，渲染结果仍为 `8080`、`/app/data`、`0.0.0.0`、`0.0.0.0:8081`，确认 `environment` 优先级覆盖生效。
- 反向用例：`.env` 缺少 `KIRO_CLIENT_TOKEN` 时 `config` 仍报 `required variable KIRO_CLIENT_TOKEN is missing a value`，必填密钥校验未被 `env_file` 削弱。
- `git diff --check` 通过。临时目录已清理。
- 未 commit、未推送；未构建镜像、未启动容器、未联网、未做真实推理验证。本次改动的运行时效果（容器内缓存折算真正生效、NewAPI 看到缓存 token）仍需在服务器上重建容器后验证。

## 上次范围（本机监听的源码 Compose 部署）
用户确认：从 stones-hub/kiro2api 源码构建，宿主机 API/管理端口仅监听回环地址，支持空账号管理模式。已修改 Compose、示例环境变量、忽略规则、README，新增 docs/deployment.md；未修改业务源码。

## 上次状态
- Git 基线：7437d4659fcc6a989592f04b616eaacb3885b793。
- 用户已明确授权提交并推送本次改动，并选择自行在服务器构建验证；不再重试本机构建。未授权远端访问或部署。
- 本次提交包含本文件；实际提交号与推送结果以 Git 日志及远端核对为准。
- 执行器：Claude Code，请求 sonnet，CLI 回报 claude-sonnet-5。
- 编码 session：64b12d62-6c86-4ad2-bb09-d826515216d2；独立只读复审：98f02fd3-2791-4ca0-b627-d17521f3cda0。
- Pi 已检查完整 diff；Compose 用合成密钥解析成功，git diff --check 成功。
- 独立复审确认回环绑定、空账号模式、持久化和健康检查与源码一致；提出构建网络阻塞和密钥不等检查缺失，已在部署文档明确。未扩展业务代码增加校验。
- Docker 构建在 Alpine apk 下载 LLVM 时超过 600 秒超时，未进入程序构建，未生成验证镜像。已确认无残留 docker build/buildx 客户端进程，未创建运行容器或测试数据。
- 未完成镜像构建、启动、健康/鉴权/持久化运行验收；浏览器后台交互及真实上游推理未验证。

## 关键限制
宿主机仅 127.0.0.1:18080（API）和 127.0.0.1:18081（管理）。容器内监听0.0.0.0是端口转发需要。首次部署需两个独立强密钥及 UID/GID 1001 可写的 data 目录。无账号可启动但不可推理。管理页面所有请求需 X-Kiro-Admin-Token，普通浏览器直接打开仍401；SSH隧道不注入认证，本次未新增受保护反代。

## 续接
说“继续处理 Compose 传递 .env 配置”，先读取本文件、`.pi/tasks/compose-env-file.md` 和 docs/deployment.md，并检查当前 Git 状态（本次改动未提交，`git status` 应显示 docker-compose.yml、.env.example、docs/deployment.md、HANDOVER.md 已修改）。

当前未获 commit/push 授权，提交推送需用户另行授权。用户自行在服务器部署验证，不擅自 SSH、不自动构建镜像或启动容器、不读取真实凭据。若需再次验证 Compose 渲染，仍用 /tmp 临时目录下的合成 .env，不要在仓库根创建 .env。

服务器侧生效步骤（交给用户执行）：在 `.env` 中取消注释 `KIRO_LOCAL_CACHE_DISCOUNT=true` 等所需变量（不要留空值），然后 `docker compose up -d --force-recreate kiro2api`，再用 `docker exec kiro2api env | cut -d= -f1 | grep -E 'KIRO_LOCAL_CACHE|KIRO_STICKY'` 核对变量名是否进入容器。
