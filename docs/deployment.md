# kiro2api 部署文档

在一台 Linux 服务器上，用 Docker Compose 从本仓库源码构建并运行 kiro2api。镜像由服务器本地构建，不使用上游预构建镜像。

文中的 `<服务器IP>`、`<SSH端口>` 等尖括号内容是占位符，执行前替换成实际值。

## 1. 整体结构

```text
NewAPI 等客户端 ──► Nginx :9002 ──► 127.0.0.1:18080 ──► 容器 API :8080
                                                          │
你的电脑浏览器 ──SSH 隧道──► Nginx 127.0.0.1:9003 ──► 127.0.0.1:18081 ──► 容器管理端 :8081
                  （注入管理员请求头）                    │
                                                  ./data（账号、Key、使用记录）
```

| 端口 | 监听位置 | 用途 |
|---|---|---|
| `18080` | 服务器 `127.0.0.1` | 容器 API，由 Compose 固定 |
| `18081` | 服务器 `127.0.0.1` | 容器管理端，由 Compose 固定 |
| `9002` | Nginx | 对外 API 入口，供 NewAPI 等客户端调用 |
| `9003` | Nginx，仅 `127.0.0.1` | 管理入口，自动注入管理员请求头，只能通过 SSH 隧道访问 |

两个容器端口只绑定服务器本机，外部无法直接访问。对外只开放 Nginx 的 API 入口；管理入口不对外开放。

## 2. 前置条件

- Linux 服务器，已安装 Git、Docker、Docker Compose v2 和 Nginx。
- 服务器能出站访问 Docker 镜像源、Alpine 软件源、Go 依赖源和 Kiro/AWS 上游。
- 你的电脑能 SSH 登录服务器。

## 3. 首次部署

以下命令都在服务器上执行，项目目录以 `/data/kiro2api` 为例。

### 3.1 拉取源码

```bash
git clone https://github.com/stones-hub/kiro2api.git /data/kiro2api && cd /data/kiro2api
```

### 3.2 准备 `.env`

```bash
cp .env.example .env && chmod 600 .env
```

生成两个随机密钥，执行两次，分别填入不同字段：

```bash
openssl rand -hex 32
```

`.env` 至少填写：

```dotenv
# 客户端调用 /v1/* 用的密钥
KIRO_CLIENT_TOKEN=第一次生成的值

# 管理端密钥，必须与上面不同，至少 32 个字符
KIRO_ADMIN_SECRET=第二次生成的值

# 管理页面来源，必须与浏览器地址栏完全一致，见第 5 节
KIRO_ADMIN_ORIGINS=http://127.0.0.1:19003

# 没有 Kiro 账号时留空，之后在管理页面添加
KIRO_AUTH_TOKEN=
```

要启用缓存显示，再加上：

```dotenv
KIRO_LOCAL_CACHE_DISCOUNT=true
```

说明：

- Compose 通过 `env_file` 把 `.env` 里的全部变量传进容器。`PORT`、`BIND_HOST`、`KIRO_ADMIN_BIND`、`KIRO_ADMIN_DATA_DIR` 由 Compose 固定，在 `.env` 里改了也不生效。
- 可选变量要么整行不写，要么写合法值。**写成空值（例如 `KIRO_LOCAL_CACHE_MAX_PERCENT=`）会导致启动失败。** 各参数的取值范围见 `.env.example`。
- 应用不会检查两个密钥是否相同，需要你自己确保不同。

### 3.3 准备数据目录

容器以 UID/GID `1001` 运行，数据目录必须归它所有：

```bash
sudo mkdir -p data && sudo chown -R 1001:1001 data && sudo chmod 700 data
```

`data/` 中保存账号凭据、API Key、模型配置、备份及 `usage/` 使用记录，已被 `.gitignore` 和 `.dockerignore` 排除。

### 3.4 构建并启动

```bash
docker compose config --quiet && docker compose up -d --build
```

查看状态和日志：

```bash
docker compose ps
```

```bash
docker compose logs --tail=100 kiro2api
```

健康检查每 30 秒执行一次，稍等一会儿状态会变为 `healthy`。日志中出现“未找到.env文件，使用环境变量”是正常提示：Compose 已通过环境变量传入配置，容器里不需要 `.env` 文件。

### 3.5 验证 API

不带密钥应返回 `401`：

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:18080/v1/models
```

带上密钥应返回模型列表：

```bash
curl -s -H "Authorization: Bearer <KIRO_CLIENT_TOKEN>" http://127.0.0.1:18080/v1/models
```

模型列表能返回，只说明服务和鉴权正常。账号池为空时不能进行模型对话。

## 4. 配置 Nginx

新建 `/etc/nginx/conf.d/kiro2api.conf`。该目录默认在 Nginx 的 `http` 块中加载；`limit_req_zone` 和 `upstream` 只能放在 `http` 块中，不能放进 `server`。

```nginx
# ===== API 入口 =====
limit_req_zone $binary_remote_addr zone=kirozone:10m rate=300r/s;

upstream kiro_backend {
    server 127.0.0.1:18080;
    keepalive 32;
}

server {
    listen 9002;
    server_name _;

    client_max_body_size 32m;
    keepalive_timeout 65s;

    proxy_connect_timeout 10s;
    proxy_send_timeout 300s;
    proxy_read_timeout 300s;   # 两次读取上游数据的间隔上限，不是整个请求的时长上限
    proxy_next_upstream off;   # 只有一个上游，不重试

    access_log /var/log/nginx/kiro-gateway-access.log;
    error_log  /var/log/nginx/kiro-gateway-error.log warn;

    location / {
        limit_req zone=kirozone burst=50 nodelay;
        limit_req_status 429;

        proxy_pass http://kiro_backend;
        proxy_http_version 1.1;

        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";

        # 流式输出（SSE）必须关闭缓冲
        proxy_buffering off;
        proxy_cache off;
        proxy_request_buffering off;
    }
}

# ===== 管理入口：只监听本机，通过 SSH 隧道访问 =====
server {
    listen 127.0.0.1:9003;
    server_name _;

    client_max_body_size 32m;

    location / {
        proxy_pass http://127.0.0.1:18081;
        proxy_http_version 1.1;

        proxy_set_header Host $http_host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";

        # 与 .env 中 KIRO_ADMIN_SECRET 完全一致；Nginx 不会读取 .env
        proxy_set_header X-Kiro-Admin-Token "替换为KIRO_ADMIN_SECRET的值";

        proxy_read_timeout 300s;
        proxy_buffering off;
        proxy_cache off;
    }
}
```

加载配置并收紧文件权限，因为配置里有管理员密钥：

```bash
sudo chmod 600 /etc/nginx/conf.d/kiro2api.conf && sudo nginx -t && sudo systemctl reload nginx
```

注意：

- **管理入口必须是 `listen 127.0.0.1:9003`。** Nginx 会替每个访问者注入管理员密钥，如果改成对外监听，任何能访问这个端口的人都拥有后台权限。
- API 入口 `9002` 对所有网卡监听，请用防火墙或云安全组只允许 NewAPI 等调用方的 IP 访问。如果 NewAPI 与 kiro2api 在同一台服务器，可以直接让它调用 `http://127.0.0.1:18080`，不需要 `9002` 这一段。
- 公网 HTTP 是明文传输。长期对公网使用，建议给 API 入口配置 HTTPS。

## 5. 访问管理后台

管理后台不需要服务器有桌面，用你自己电脑的浏览器访问。

### 5.1 在你的电脑上建立 SSH 隧道

**在你自己的电脑上执行，不是在服务器上：**

```bash
ssh -p <SSH端口> -N -o ExitOnForwardFailure=yes -L 127.0.0.1:19003:127.0.0.1:9003 root@<服务器IP>
```

- SSH 不是 `22` 端口时必须带 `-p`。
- 输入密码后窗口停住、没有任何输出是正常的，`-N` 表示只建隧道，不打开命令行。保持窗口不要关。
- 需要正常登录服务器时，另开一个终端。结束隧道时在隧道窗口按 `Ctrl+C`。

### 5.2 打开后台

浏览器打开：

```text
http://127.0.0.1:19003/
```

**必须与 `.env` 中的 `KIRO_ADMIN_ORIGINS` 完全一致**，包括协议、主机名和端口。用 `localhost`、公网 IP 或其他端口访问，添加账号等操作会提示“请求来源不允许”。

如果本地 `19003` 被占用，换成其他端口，同时把 `KIRO_ADMIN_ORIGINS` 改成对应地址，再执行：

```bash
docker compose up -d --force-recreate kiro2api
```

说明：当前源码只接受 HTTPS 来源，或 `127.0.0.1`、`localhost` 的 HTTP 来源。因此不能直接用 `http://<服务器IP>:9003` 访问管理后台，这就是要走 SSH 隧道的原因。

### 5.3 在后台完成初始化

1. 添加 Kiro 账号，支持 Github/Google 社交登录及 IdC 账号，保存后会自动检查额度。
2. 在 API Key 页面为 NewAPI 创建独立的 Key。Key 只显示一次，请立即保存。

`KIRO_CLIENT_TOKEN` 也能调用 API，作为内置 Key 保留；给不同调用方分别创建 Key，便于分开统计和停用。

## 6. 对接 NewAPI

在 NewAPI 中新增渠道：

| 项目 | 填写 |
|---|---|
| 渠道类型 | Anthropic Claude（走 `/v1/messages`）或 OpenAI（走 `/v1/chat/completions`） |
| Base URL | `http://<服务器IP>:9002`，与 NewAPI 同机时可用 `http://127.0.0.1:18080` |
| 密钥 | 后台创建的 API Key |
| 模型 | 与 kiro2api 模型列表中的名称一致 |

kiro2api 同时接受 `Authorization: Bearer <Key>` 和 `x-api-key: <Key>`。

### 缓存 token 显示

需要在 `.env` 中设置 `KIRO_LOCAL_CACHE_DISCOUNT=true` 并重建容器。命中后：

- Anthropic 格式返回 `cache_read_input_tokens`。
- OpenAI 格式返回 `prompt_tokens_details.cached_tokens`。
- 流式和非流式响应都会返回。

命中条件是三个同时满足：5 分钟内、同一个 API Key（请求带 `metadata.user_id` 时还要同一个用户）、请求前缀与上一次相同。**第一次请求不会显示缓存**，同一对话的第二轮起才可能显示。

这是 kiro2api 本地折算的计费数据，不是 Kiro 上游返回的真实缓存用量。

核对缓存变量是否已传入容器，只列变量名，不打印值：

```bash
docker exec kiro2api env | cut -d= -f1 | grep -E 'KIRO_LOCAL_CACHE|KIRO_STICKY'
```

没有输出说明变量没进容器：检查 `.env` 中对应行是否被注释，然后重建容器。

## 7. 日常运维

### 更新代码

```bash
cd /data/kiro2api && git pull --ff-only && docker compose up -d --build
```

`git pull` 提示服务器有本地改动时先停下来，不要强制覆盖。

### 只修改了 `.env`

不需要重新构建，但必须重建容器，`docker compose restart` 不会更新环境变量：

```bash
docker compose up -d --force-recreate kiro2api
```

### 查看日志

```bash
docker compose logs -f --tail=100 kiro2api
```

### 备份

备份整个 `data/`，不要只备份 `accounts.json`。先停服务，打包到仓库外的目录，再启动。`/path/to/backups` 替换为实际目录：

```bash
docker compose stop kiro2api
```

```bash
sudo tar -czf /path/to/backups/kiro2api-data-$(date +%Y%m%d%H%M%S).tar.gz data
```

```bash
docker compose up -d kiro2api
```

### 禁止操作

- 不要执行 `docker compose down -v`，也不要删除 `data/`。
- 不要对 `data/` 使用 `chmod 777`。
- 不要把 `.env`、`data/` 或含管理员密钥的 Nginx 配置提交到 Git。

## 8. 常见问题

| 现象 | 原因 | 处理 |
|---|---|---|
| `open /app/data/.accounts-*.tmp: permission denied` | `data/` 不归 UID `1001` 所有 | 停容器，执行 3.3 节的 `chown`，再启动 |
| `未找到KIRO_AUTH_TOKEN环境变量` | 未启用管理模式，通常是使用了旧版 `docker-compose.yml` | 拉取最新代码；当前 Compose 固定启用管理模式，空账号也能启动 |
| `KIRO_CLIENT_TOKEN is required` | `.env` 缺少必填密钥 | 补齐 `KIRO_CLIENT_TOKEN` 和 `KIRO_ADMIN_SECRET` |
| 日志提示“未找到.env文件，使用环境变量” | 正常提示 | 无需处理 |
| 容器反复重启，可选变量相关报错 | `.env` 中可选变量写了空值或非法值 | 删除该行或改成合法值 |
| 状态 `unhealthy` | 健康检查带密钥访问 `/v1/models` 失败 | 查看日志；确认 `KIRO_CLIENT_TOKEN` 已设置 |
| 后台提示“需要管理员认证” | 请求没带上正确的管理员头 | 确认通过 9003 入口访问，Nginx 中的密钥与 `KIRO_ADMIN_SECRET` 一致 |
| 添加账号提示“请求来源不允许” | 浏览器地址与 `KIRO_ADMIN_ORIGINS` 不一致 | 按第 5 节对齐地址后重建容器 |
| 隧道命令 `Operation timed out` | 没带 `-p` 或端口不对，或网络临时不通 | 确认能用相同端口正常 SSH 登录 |
| 浏览器打不开 `127.0.0.1:19003` | 隧道未建立，或本地端口被占用 | 检查隧道窗口有无报错；在服务器执行 `curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:9003/`，返回 200 说明服务器侧正常 |
| NewAPI 不显示缓存 | 未启用缓存折算，或请求未命中 | 按第 6 节核对变量，用同一 Key 在 5 分钟内发同一对话的第二轮 |
| API 返回 401 | 密钥错误或未携带 | 使用后台创建的 Key 或 `KIRO_CLIENT_TOKEN` |
