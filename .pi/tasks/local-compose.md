# 本机监听的源码 Compose 部署

- 用户已确认修改，未授权 commit/push/部署。
- 基线：7437d4659fcc6a989592f04b616eaacb3885b793，工作区干净。
- 执行器：Claude Code；请求模型：sonnet。
- Risk：关键（部署、安全监听、凭据配置）。

## Scope
仅修改 docker-compose.yml、.env.example、.gitignore、.dockerignore、README.md，可新增 docs/deployment.md；不改 Go/JS 或其他业务源码。
1. Compose 从当前源码 build，image kiro2api-local:latest；宿主机仅 127.0.0.1:18080:8080 和 127.0.0.1:18081:8081。容器内 BIND_HOST=0.0.0.0、PORT=8080、KIRO_ADMIN_BIND=0.0.0.0:8081 固定。
2. 默认管理模式 KIRO_ADMIN_DATA_DIR=/app/data，空账号可启动。两个密钥由 .env 提供并要求非空（管理密钥应用要求>=32），不得弱默认；KIRO_AUTH_TOKEN 可空。KIRO_ADMIN_ORIGINS 默认 http://127.0.0.1:18081。仅显式传递必要环境变量，不把整份 .env 无限制传入。挂载 ./data:/app/data，45s stop_grace_period，日志轮转，认证健康检查使用容器内变量 $$。
3. .env.example 精简到与默认管理模式一致，默认空账号，无真实凭据或弱密钥；解释客户端/管理员独立密钥、Origins 和端口。保留合理可选配置。
4. gitignore/dockerignore 排除 data/（含 jsonl 和备份，防入库/进入构建上下文）；dockerignore 同时排除 .pi/ 和 HANDOVER.md，以及已有 .env 文件。
5. 更新 README 对冲突的 Docker/健康检查/认证说明，不再建议默认使用原作者镜像；删除 docker compose down -v 等危险排障指令。写 docs/deployment.md：Git clone/固定SHA；首次 cp env、两次 openssl rand -hex 32、mkdir data/chown1001:1001/chmod700；up --build；端口仅服务器本机，客户端通过SSH隧道；空账号不可推理；管理页面所有请求需X-Kiro-Admin-Token，SSH隧道本身不会注入，不虚构可直接登录，需受保护反代（本任务不新增反代）。备份整个data、不得down -v、健康不等于推理可用、真实凭据不提交。所有供复制shell命令单行。

## Verify
执行器仅静态检查 docker compose config（使用合成密钥，不写真实.env）、git diff --check；不 commit/push/启动/联网/读真实凭据。
Pi 独立检查完整diff，独立Claude review新会话；本机隔离构建/启动，合成密钥、空账号、临时数据、回环独立端口，验证无凭据401、认证models成功、管理鉴权、重建数据保留、健康和日志；不连真实上游。管理网页浏览器交互和真实推理因缺受控反代/真实账号不作为已验证。
