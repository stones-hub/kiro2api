# Compose 传递 .env 全部配置（修复缓存配置不生效）

- 用户已授权修改；未授权 commit/push/部署。
- 基线：d64c93e849340f9468f358c005988502138c856b，工作区仅本任务书未跟踪。
- 执行器：Claude Code；请求模型：opus（sonnet 首次调用报模型不可用，用户选择换模型重试）。
- Risk：普通（部署配置，不改业务代码）。

## 问题
docker-compose.yml 只用 environment 显式白名单传变量，服务器 .env 中的 KIRO_LOCAL_CACHE_DISCOUNT、KIRO_LOCAL_CACHE_MAX_PERCENT、KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT、KIRO_LOCAL_CACHE_MIN_UNCACHED_TOKENS、KIRO_STICKY_WAIT_SECONDS、KIRO_MODELS_FILE 等不会进入容器，导致本地缓存折算关闭，NewAPI 看不到缓存 token。
不能用 `${VAR:-}` 逐个透传：server/cache_discount.go 与 main.go 用 LookupEnv，变量存在但为空会解析失败退出。

## Scope
只改 docker-compose.yml、.env.example、docs/deployment.md、README.md（如有相关说明）、HANDOVER.md。不改 Go/JS 源码。
1. docker-compose.yml 增加 `env_file: - .env`，保留现有 environment 固定值（BIND_HOST、PORT、KIRO_ADMIN_BIND、KIRO_ADMIN_DATA_DIR 及必填密钥校验），environment 优先级高于 env_file，所以端口/数据目录不会被 .env 覆盖。其余已有条目保持原样。不要改端口、挂载、健康检查。
2. .env.example 新增“本地缓存折算（可选）”段，全部注释掉，说明：
   - KIRO_LOCAL_CACHE_DISCOUNT=true 才启用；默认关闭。
   - KIRO_LOCAL_CACHE_MAX_PERCENT 默认 40（0-100）。
   - KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT 默认 true，只能 true/false。
   - KIRO_LOCAL_CACHE_MIN_UNCACHED_TOKENS 默认 0（>=0）。
   - 命中条件：5 分钟内、同一 API Key（及 metadata.user_id）、请求前缀重复；首次请求不会显示缓存。这是本地折算，不是上游真实缓存计量。
   - 这些变量要么不写，要么写合法值；写成空值（如 `KIRO_LOCAL_CACHE_MAX_PERCENT=`）会导致启动失败。
   - 同时补充 KIRO_STICKY_WAIT_SECONDS（可选，0-60 整数，续接对话等待原账号的秒数；空值或非法值启动失败）。
   - 同步修正 KIRO_ADMIN_ORIGINS 注释：值必须与浏览器地址栏来源完全一致（协议+主机+端口），例如通过 SSH 隧道本地端口 19003 访问则填 http://127.0.0.1:19003。
3. docs/deployment.md 说明：Compose 会把 .env 全部变量传进容器，但端口、监听地址、数据目录由 compose 固定覆盖；新增缓存配置简述及修改 .env 后需 `docker compose up -d --force-recreate kiro2api`（restart 不更新环境变量）；如何仅列变量名核对：`docker exec kiro2api env | cut -d= -f1 | grep -E 'KIRO_LOCAL_CACHE|KIRO_STICKY'`。同时修正 KIRO_ADMIN_ORIGINS 与浏览器访问地址匹配的说明。命令单行。
4. HANDOVER.md 更新本次状态（基线、改动、验证、未提交未推送）。

## Verify
- 用临时目录下的合成 .env（不要在仓库创建 .env），例如复制仓库到 /tmp 或使用 `docker compose --env-file`/`--project-directory` 方式，执行 docker compose config，确认：env_file 生效后 KIRO_LOCAL_CACHE_DISCOUNT 出现在渲染结果；.env 中写 PORT=9999、KIRO_ADMIN_DATA_DIR=/x 时渲染结果仍为 8080、/app/data。
- git diff --check。
- 不 commit/push，不构建镜像，不启动服务，不联网，不读取真实凭据。清理临时文件。
