# 本机监听的源码 Compose 部署

本文档描述如何在**本机**使用 Docker Compose，从当前源码构建并运行 kiro2api，仅监听本机回环地址，不对外网开放。

## 1. 获取源码

建议固定到某个确定的提交（commit SHA），而不是跟随默认分支漂移：

```bash
git clone https://github.com/stones-hub/kiro2api.git kiro2api && cd kiro2api
```

记录并确认 `git rev-parse HEAD` 对应本次批准部署的提交；若需切换固定版本，先把目标提交号填入 `git checkout` 命令再执行，不要原样复制带尖括号的占位符。

## 2. 准备环境变量

复制示例文件：

```bash
cp .env.example .env
```

生成两个独立的随机密钥（客户端密钥与管理密钥不能相同，管理密钥至少 32 字符）：

```bash
openssl rand -hex 32
```

```bash
openssl rand -hex 32
```

将两次输出分别填入 `.env` 的 `KIRO_CLIENT_TOKEN` 和 `KIRO_ADMIN_SECRET`。应用不会自动检查两个值是否相同，部署者必须确认使用两个不同的随机值。`KIRO_AUTH_TOKEN` 可以留空，账号池为空也能正常启动，后续通过管理页面添加账号。

`.env` 里是明文密钥，收紧文件权限：

```bash
chmod 600 .env
```

`docker-compose.yml` 通过 `env_file: - .env` 把 `.env` 里的**全部**变量传进容器，不需要在 `environment` 里逐个列出新变量。但 `environment` 的优先级高于 `env_file`，所以下面这几项由 Compose 固定覆盖，在 `.env` 里改它们不起作用：

- `PORT=8080`、`BIND_HOST=0.0.0.0`、`KIRO_ADMIN_BIND=0.0.0.0:8081` — 容器内部监听，宿主机端口由 `ports` 映射决定
- `KIRO_ADMIN_DATA_DIR=/app/data` — 容器内数据目录，对应挂载的 `./data`

`.env` 里的可选变量要么整行不写（用代码里的默认值），要么写合法值。程序用 `LookupEnv` 读取 `KIRO_LOCAL_CACHE_*` 和 `KIRO_STICKY_WAIT_SECONDS`：变量存在但值为空（如 `KIRO_LOCAL_CACHE_MAX_PERCENT=`）会解析失败并直接退出，容器起不来。各变量的取值范围见 `.env.example`，这里只说效果：

- `KIRO_LOCAL_CACHE_DISCOUNT=true` 才启用本地缓存折算，默认关闭。启用后，命中的请求会把一部分 input token 记成 `cache_read`，下游（如 NewAPI）才能看到缓存 token。
- 命中需要同时满足：5 分钟内、同一 API Key（及 `metadata.user_id`）、请求前缀重复。首次请求不会显示缓存。
- 这是本地计费折算，不是上游的真实缓存计量，上游实际消耗不受它影响。
- `KIRO_STICKY_WAIT_SECONDS` 控制续接对话等待原账号的秒数（默认 10），上游 prompt cache 按账号隔离，换账号会让下一轮消耗大致翻倍。

改完 `.env` 必须重建容器才会生效，`docker compose restart` 不会重新读取环境变量：

```bash
docker compose up -d --force-recreate kiro2api
```

`KIRO_AUTH_TOKEN` 默认只支持两种取值：留空，或直接写 JSON 字符串。如果想把它设置为"容器内文件路径"，那个路径必须真实存在于容器里，而本 `docker-compose.yml` 只挂载了 `./data:/app/data`，并不会自动挂载任意主机路径；要用文件路径方式，需要自己在 `docker-compose.yml` 里额外加一条 volumes 挂载（本任务未新增，默认场景下不要配置成文件路径）。

## 3. 准备数据目录

```bash
sudo mkdir -p data && sudo chown 1001:1001 data && sudo chmod 700 data
```

容器内进程以 uid/gid `1001:1001`（`appuser`）运行，`data/` 目录权限必须正确，否则容器无法读写账号池和使用记录，会启动失败或静默丢数据。

`data/` 用于持久化账号池配置（`accounts.json`）、API Key、模型配置及备份，以及 `usage/` 下按天滚动的使用记录（`usage-*.jsonl`），已在 `.gitignore`/`.dockerignore` 中排除，不会入库或进入构建上下文。

## 4. 构建并启动

构建需要出站访问 Docker 镜像源、Alpine 软件源和 Go 依赖源。本次本机验证在下载 Alpine 构建工具链（LLVM）时超过 600 秒而中断，尚未完成镜像构建或容器运行验收；这不代表目标服务器必然失败。若构建长时间无进展，应先检查构建日志和服务器网络，不要反复重启应用或随意替换不可信软件源。

```bash
docker compose up --build -d
```

服务只绑定在本机回环地址：

- `127.0.0.1:18080` — API 端口（`/v1/*`）
- `127.0.0.1:18081` — 管理页面端口（`/`、静态资源 `/assets/*`、管理接口 `/api/*`、`/healthz`）

容器内部固定使用 `BIND_HOST=0.0.0.0`、`PORT=8080`、`KIRO_ADMIN_BIND=0.0.0.0:8081`，这是容器内部地址，外部只能通过上面两个回环端口访问。

## 5. 从远程机器访问（SSH 隧道）

如果 kiro2api 运行在一台远程主机上，不要直接对外暴露端口。可以用 SSH 隧道把远程的回环端口转发到本地：

```bash
ssh -N -L 18080:127.0.0.1:18080 -L 18081:127.0.0.1:18081 user@remote-host
```

上面的命令把远程端口映射到本地同号端口。如果本地端口被占用，换成别的端口（例如 `-L 19003:127.0.0.1:18081`），那么 `KIRO_ADMIN_ORIGINS` 也要跟着改：它的值必须与浏览器地址栏里的来源**完全一致**，协议、主机、端口三者都要对得上。用本地 19003 访问管理页面就填 `http://127.0.0.1:19003`；填成 `http://127.0.0.1:18081`（端口不符）或 `http://localhost:19003`（主机名不符）都算不同来源，会被拒绝。

隧道建立后，本地访问 `http://127.0.0.1:18080` 和 `http://127.0.0.1:18081` 等效于直接在远程机器本机访问端口。但这**只是网络层的端口转发**，隧道本身不注入、不携带任何认证信息：浏览器直接打开 `http://127.0.0.1:18081` 仍然会收到 401（管理页面要求 `X-Kiro-Admin-Token` 请求头，见下一节），不存在"隧道一建好页面就能用"的效果。本文档没有验证过通过隧道在浏览器里正常使用管理页面（需要浏览器能带上自定义头，通常要靠插件或受保护的反向代理，本任务未新增），下节提供命令行 curl 的验证步骤，尚未完成本次容器的运行验证。

## 6. 验证服务

账号池为空时，服务可以启动，但无法完成真实推理（没有可用的上游账号）。以下验证的是服务本身是否正常运行，不代表已验证真实推理，也不代表管理页面可以在浏览器里直接使用：

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/v1/models
```

应返回 `401`（未提供认证）。带上客户端密钥应返回 `200`：

```bash
curl -s -H "Authorization: Bearer <KIRO_CLIENT_TOKEN>" http://127.0.0.1:18080/v1/models
```

管理端口（18081）上的**所有**路由都需要在请求头中携带 `X-Kiro-Admin-Token: <KIRO_ADMIN_SECRET>`，没有这个头一律 401——这既包括管理接口 `/api/*`，也包括页面本身 `/`、静态资源 `/assets/*`，以及 `/healthz`：

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18081/healthz
```

上面这条不带头，预期返回 `401`。带上管理密钥才会返回 `200`：

```bash
curl -s -H "X-Kiro-Admin-Token: <KIRO_ADMIN_SECRET>" http://127.0.0.1:18081/healthz
```

浏览器直接访问 `http://127.0.0.1:18081/` 同样会被这个全局中间件拦成 401，因为普通浏览器导航请求带不上自定义头。管理页面当前没有配置受保护的反向代理（本任务未新增反代），不要在没有额外保护的情况下把 18081 端口暴露到公网或不受信任网络；直接浏览器交互和真实推理流程本任务未做验证。

## 7. 常用操作

查看日志：

```bash
docker compose logs -f kiro2api
```

查看健康状态：

```bash
docker compose ps
```

核对容器里到底有哪些可选变量（只列变量名，不打印值，避免泄露密钥）：

```bash
docker exec kiro2api env | cut -d= -f1 | grep -E 'KIRO_LOCAL_CACHE|KIRO_STICKY'
```

没有输出说明这些变量没进容器：确认 `.env` 里相应行没被注释掉，并用下面的命令重建容器（`docker compose restart` 不会更新环境变量）：

```bash
docker compose up -d --force-recreate kiro2api
```

备份数据：先停止服务，再把整个 `data/` 目录打包到**仓库外**的路径（不要在仓库目录下生成 `data.backup*` 之类的新目录，容易变成未被忽略、误入库的内容），最后再启动服务。下面的 `/path/to/backups` 只是占位符，替换成你机器上一个仓库之外、你有权限写入的目录：

```bash
docker compose stop
```

```bash
sudo tar -czf /path/to/backups/kiro2api-data-$(date +%Y%m%d%H%M%S).tar.gz data
```

```bash
docker compose up -d
```

重新构建并启动（代码变更后，保留 `./data` 数据目录）：

```bash
docker compose up --build -d
```

## 8. 注意事项

- 不要使用 `docker compose down -v`：这会删除命名数据卷。本部署的账号数据在挂载的 `./data` 目录中，`down -v` 不会直接删除它，但仍建议统一使用 `docker compose stop` / `docker compose up -d` 来避免误操作，排障时按上面的"常用操作"执行，不要图省事直接删数据卷或数据目录。
- 健康检查通过（HTTP 200）只表示 API 进程存活、客户端密钥校验通过，不代表账号池里有可用账号，也不代表能完成真实推理。
- 不要把真实的 `refreshToken`、`clientId`、`clientSecret` 等凭据提交到仓库或写入示例文件；`.env` 已被 `.gitignore` 排除。
