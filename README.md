# kiro2api

把 Kiro（AWS CodeWhisperer）账号转换成标准的 Anthropic / OpenAI 接口。Claude Code、NewAPI 以及其他 OpenAI 兼容客户端都能直接接入，上游只对接 Kiro。

- 两套接口：`/v1/messages`（Anthropic 格式）和 `/v1/chat/completions`（OpenAI 格式），都支持流式和非流式。
- 多账号池：在管理后台添加 Kiro 账号。同一个对话会固定在同一个账号上，新对话自动分散到空闲账号，账号出错时自动换号重试一次。
- 管理后台：账号、代理、模型、API Key、使用日志、错误日志，都在网页上操作。

Go 1.24 · gin · 镜像 `ghcr.io/stones-hub/kiro2api`（linux/amd64、linux/arm64）

---

## 目录

- [两种运行模式](#两种运行模式)
- [快速开始](#快速开始)
- [客户端接入](#客户端接入)
- [API 接口](#api-接口)
- [模型](#模型)
- [请求转换做了什么](#请求转换做了什么)
- [多账号调度](#多账号调度)
- [管理后台](#管理后台)
- [环境变量](#环境变量)
- [排查问题](#排查问题)
- [开发](#开发)
- [相关文档](#相关文档)

---

## 两种运行模式

| | 管理模式（推荐） | 环境变量模式（旧） |
|---|---|---|
| 怎么开启 | 设置 `KIRO_ADMIN_DATA_DIR` | 不设置 `KIRO_ADMIN_DATA_DIR`，只设置 `KIRO_AUTH_TOKEN` |
| 账号从哪来 | 管理后台添加，保存在数据目录的 `accounts.json` | `KIRO_AUTH_TOKEN` 里的 JSON 或文件 |
| 账号选择 | 会话粘滞 + 按空位分散 + 出错换号重试 | 按配置顺序依次使用 |
| 客户端密钥 | `KIRO_CLIENT_TOKEN`，以及后台创建的多个 API Key | 只有 `KIRO_CLIENT_TOKEN` |
| 管理后台、使用日志、错误日志、代理 | 有 | 没有（只有主端口上一个只读的 `/api/tokens`，不需要认证） |

本仓库的 `docker-compose.yml` 和下面的说明都按管理模式写。管理模式下账号池为空也能启动，在后台添加账号后才能真正调用模型。

---

## 快速开始

### 方式一：使用发布好的镜像

适合在其他项目或服务器上直接升级使用，不需要源码。在 compose 文件里写：

```yaml
services:
  kiro2api:
    image: ghcr.io/stones-hub/kiro2api:v1.2.0   # 写死版本号，方便回退
    container_name: kiro2api
    restart: unless-stopped
    ports:
      - "127.0.0.1:18080:8080"   # API
      - "127.0.0.1:18081:8081"   # 管理后台
    env_file:
      - .env
    environment:
      - BIND_HOST=0.0.0.0
      - PORT=8080
      - KIRO_ADMIN_BIND=0.0.0.0:8081
      - KIRO_ADMIN_DATA_DIR=/app/data
    volumes:
      - ./data:/app/data
    stop_grace_period: 45s
```

准备 `.env` 和数据目录。`.env` 至少要有 `KIRO_CLIENT_TOKEN`、`KIRO_ADMIN_SECRET`、`KIRO_ADMIN_ORIGINS` 三项，模板见 [.env.example](./.env.example)。

```bash
openssl rand -hex 32   # 运行两次，分别作为 KIRO_CLIENT_TOKEN 和 KIRO_ADMIN_SECRET
sudo mkdir -p data && sudo chown 1001:1001 data && sudo chmod 700 data   # 容器内以 uid 1001 运行
docker compose pull && docker compose up -d   # 拉取镜像并启动
```

升级时把 `image` 改成新版本号，然后再执行一次 `docker compose pull && docker compose up -d`。可用版本见 GitHub 仓库的 Tags。

### 方式二：从源码构建

```bash
git clone https://github.com/stones-hub/kiro2api.git && cd kiro2api
cp .env.example .env && chmod 600 .env   # 然后编辑 .env
sudo mkdir -p data && sudo chown 1001:1001 data && sudo chmod 700 data
docker compose up --build -d
```

仓库自带的 `docker-compose.yml` 只把端口绑定在本机：API 是 `127.0.0.1:18080`，管理后台是 `127.0.0.1:18081`。如何生成密钥、配置 Nginx 对外提供 API、通过 SSH 隧道访问后台、对接 NewAPI、日常运维，完整步骤见 [docs/deployment.md](./docs/deployment.md)。

### 方式三：本机直接运行

```bash
go build -o kiro2api main.go
KIRO_CLIENT_TOKEN=换成随机串 KIRO_ADMIN_SECRET=换成至少32位的随机串 KIRO_ADMIN_DATA_DIR=./data KIRO_ADMIN_ORIGINS=http://127.0.0.1:8081 ./kiro2api
```

程序启动时会自动读取当前目录下的 `.env`。默认 API 监听 `:8080`，管理后台监听 `127.0.0.1:8081`。

### 启动后检查

```bash
curl -H "Authorization: Bearer $KIRO_CLIENT_TOKEN" http://127.0.0.1:18080/v1/models   # 返回模型列表说明服务正常
```

### 访问管理后台

管理端口上的所有请求，包括页面本身，都必须带 `X-Kiro-Admin-Token: <KIRO_ADMIN_SECRET>` 请求头，否则返回 401。浏览器没法自己加这个头，所以要在前面放一个只监听本机的 Nginx 注入它，再通过 SSH 隧道访问。另外，`KIRO_ADMIN_ORIGINS` 必须和浏览器地址栏里的来源完全一致，协议、主机、端口都要对上。具体配置见 [docs/deployment.md](./docs/deployment.md) 第 4、5 节。不要把管理端口暴露到公网。

---

## 客户端接入

**Claude Code**

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:18080   # 不要带 /v1，Claude Code 会自己拼路径
export ANTHROPIC_AUTH_TOKEN=你的API Key
claude
```

**NewAPI**：新建渠道，类型选 Anthropic Claude（走 `/v1/messages`）或 OpenAI（走 `/v1/chat/completions`）。Base URL 填 kiro2api 的地址，密钥填 API Key，模型名要和 `/v1/models` 返回的名字一致。缓存 token 的显示方式见 [docs/deployment.md](./docs/deployment.md) 第 6 节。

**curl**

```bash
curl http://127.0.0.1:18080/v1/messages -H "Authorization: Bearer $KIRO_CLIENT_TOKEN" -H "Content-Type: application/json" -d '{"model":"claude-sonnet-4.6","max_tokens":200,"messages":[{"role":"user","content":"你好"}]}'
curl http://127.0.0.1:18080/v1/chat/completions -H "Authorization: Bearer $KIRO_CLIENT_TOKEN" -H "Content-Type: application/json" -d '{"model":"claude-sonnet-4.6","stream":true,"messages":[{"role":"user","content":"你好"}]}'
```

---

## API 接口

主端口（默认 8080）提供的接口：

| 接口 | 说明 |
|---|---|
| `POST /v1/messages` | Anthropic Messages 格式，支持流式和非流式 |
| `POST /v1/chat/completions` | OpenAI Chat Completions 格式，支持流式和非流式 |
| `POST /v1/messages/count_tokens` | 本地估算 token 数，不调用上游 |
| `GET /v1/models` | 当前可用的模型列表，包括别名 |
| `GET /api/tokens` | 只在环境变量模式下存在：只读的账号状态，不需要认证 |

**认证**：所有 `/v1/*` 接口都要带 `Authorization: Bearer <Key>` 或 `x-api-key: <Key>`。Key 可以是 `KIRO_CLIENT_TOKEN`，也可以是管理后台创建的 API Key（仅管理模式）。

**限制和错误**：
- 请求体最大 32 MiB。
- 模型名不在列表里时，直接返回 400 `model_not_found`。
- 账号并发全部占满时返回 429，并带 `Retry-After: 2`。
- 没有可用账号时返回 503。
- 上游的错误会转换成对应的状态码，用户能看到的错误文案里不出现 "Kiro"。

**响应头**：每个响应都带 `X-Request-ID`（客户端自己传了就沿用），用它可以在日志和"错误日志"页里找到对应的记录。另外有一些 `X-Kiro-*` 响应头，用来说明这次请求实际生效的处理：
- `X-Kiro-Thinking-*`：实际采用的思考模式和档位。
- `X-Kiro-Max-Tokens-Adjusted`、`X-Kiro-Effective-Max-Tokens`：`max_tokens` 被调整过时出现。
- `X-Kiro-Cache-Accounting`：开启本地缓存折算后出现。

---

## 模型

内置模型列表在 [config/models.default.json](./config/models.default.json)，格式和 Kiro 的 `ListAvailableModels` 接口一致。目前包含：

- Claude：opus-5、sonnet-5、opus-4.8/4.7/4.6/4.5、sonnet-4.6/4.5/4、haiku-4.5
- 其他：auto、gpt-5.6（sol/terra/luna）、deepseek-3.2、minimax-m2.5/m2.1、glm-5、qwen3-coder-next

实际能用哪些，以你账号在 Kiro 那边开放的为准。

**别名**：
- 版本号里的点也可以写成横线，例如 `claude-opus-4-8` 等于 `claude-opus-4.8`。
- 带日期的旧名字也能用，例如 `claude-sonnet-4-20250514`、`claude-sonnet-4-5-20250929`、`claude-haiku-4-5-20251001`、`claude-opus-4-5-20251101`。
- 任意模型名后面加 `-thinking`，表示打开思考。

**更新模型列表**，生效优先级从高到低：
1. 管理后台"模型"页的"检测线上模型"：用一个账号查询 Kiro 当前开放的模型，勾选后立即生效，保存在数据目录的 `models.json`。
2. `KIRO_MODELS_FILE`：启动时读取的模型列表文件，格式不合法会导致启动失败。
3. 内置列表。

**思考（thinking）**：支持 Anthropic 的 `thinking`（`adaptive`、`enabled`、`disabled`，以及 `budget_tokens`）、`output_config.effort`，OpenAI 的 `reasoning_effort`，以及上面说的 `-thinking` 后缀。不同模型的能力（原生自适应、reasoning、旧版提示词方式）以模型列表里的 schema 为准，`/v1/models` 返回的 `thinking_mode`、`reasoning_efforts` 字段可以查到。

---

## 请求转换做了什么

客户端请求会先转成 Kiro 的格式再发出去。转换过程中会做下面这些处理，每一项都可以在[环境变量](#环境变量)里调整：

| 处理 | 默认 | 说明 |
|---|---|---|
| 身份声明 | 开启 | 在 system 最前面加一段英文声明，让模型回答"我是 Claude，由 Anthropic 开发"，而不是自称 Kiro。只能降低自称 Kiro 的概率，不能保证完全不出现。`KIRO_IDENTITY_PROMPT=off` 关闭，填其他值则作为自定义声明。详见 [docs/identity-prompt.md](./docs/identity-prompt.md) |
| system 敏感词清洗 | 开启 | 客户端 system 里，含 `Claude Code`、`Claude Agent SDK` 等词的行整行删掉，然后继续请求。用户消息、历史消息、工具内容都不处理。`KIRO_SYSTEM_SENSITIVE_WORDS=off` 关闭，填文件路径可以追加自定义词。详见 [docs/system-sanitize.md](./docs/system-sanitize.md) |
| 图片 | — | 支持 base64 和 `data:` URL 形式的 jpeg/png/gif/webp，单张最大 20 MB。不会去下载远程 http 图片。无效的图片会被跳过并在原位置留一句说明，不会让整个请求失败 |
| 文档（PDF / 文本） | — | Kiro 不收 document 块，所以 PDF 会被转成纯文本发过去（版式、图片、图表会丢失），文本文件直接发文本。单个文档最多保留 40 万字符，超出截断；不支持的类型会留一句说明 |
| WebSearch | — | Claude Code 的 WebSearch 子请求（只带一个原生 `web_search` 工具）会改用 Kiro 的 MCP 接口真实搜索，再按 Anthropic 格式拼成搜索结果。其他不支持的工具见 [docs/UNSUPPORTED_TOOLS.md](./docs/UNSUPPORTED_TOOLS.md) |
| 输出写满 | — | Kiro 写满输出上限时会发 `ContentLengthExceededException`。四种请求方式都会把它转成"被截断"：Anthropic 格式返回 `stop_reason: max_tokens`，OpenAI 格式返回 `finish_reason: length` |
| 工具描述长度 | 10000 字节 | 超过的部分截断，避免上游报错，用 `MAX_TOOL_DESCRIPTION_LENGTH` 调整 |

---

## 多账号调度

只在管理模式下生效。完整规则见 [docs/account-scheduling.md](./docs/account-scheduling.md)。

- **会话粘滞**：续接的对话会回到上一轮的账号，因为 Kiro 的 prompt cache 是按账号隔离的，换账号会让这一轮的消耗大约翻倍。识别同一会话的依据是：API Key、`metadata.user_id`、模型和消息前缀，记录保留 5 分钟。原账号只是并发满了时，最多等 `KIRO_STICKY_WAIT_SECONDS` 秒（默认 10 秒）；原账号如果停用、额度用完或正在冷却，就立即换号。
- **新对话分配**：先按后台设置的"分配顺序"（账号类型优先级）分组，同一组里优先选剩余空位最多的账号，空位一样多时随机选。
- **出错换号重试**：上游返回 401、402、403、429、5xx，或者出现网络错误、代理错误时，自动换另一个账号重试 1 次。这只发生在客户端还没收到任何内容之前；400 这类请求本身有问题的情况不重试。
- **冷却和停用**：连续收到 402 或 429 时进入冷却，冷却时间从 1 分钟开始翻倍，最长 30 分钟。月度请求次数用完时冷却 6 小时，或者等到官方重置时间（取更早的那个）。账号的官方订阅变成免费版时，会被自动停用。
- **并发**：每个账号有并发上限，流式输出期间也占着一个并发名额。

---

## 管理后台

| 页面 | 能做什么 |
|---|---|
| 运行总览 | 请求数、错误数、延迟、模型分布，可以看近 24 小时或近 31 天，也可以按 API Key 筛选 |
| 账号池 | 添加账号（单个添加，或粘贴 JSON 批量导入，支持 Github / Google 社交登录、IdC，以及 Kiro 导出的 JSON）；设置并发上限和出口代理；启用、停用、删除、还原；批量操作；更新官方额度；设置按账号类型的分配顺序 |
| 代理 | SOCKS5 出口代理，可以测试连通性。账号的刷新凭据、查询额度、发送请求都会走它指定的代理 |
| 模型 | 查看当前模型列表；检测线上模型，勾选后应用；移除模型 |
| 使用日志 | 每次请求一条记录：时间、Key、账号、模型、token、缓存、Kiro 实际扣的积分、耗时。按北京时间分天保存 31 天 |
| 错误日志 | 每次请求出错时保存完整现场：客户端请求、发给 Kiro 的请求、Kiro 的响应，各含请求头和请求体，可以分段复制，也可以一键复制全部。密钥会打码，对话内容是明文。只保留最新 200 条。详见 [docs/error-dump.md](./docs/error-dump.md) |
| API Key | 创建调用 `/v1` 用的 Key。Key 只在创建时显示一次，服务器只保存它的哈希。可以停用、删除，用量按 Key 单独统计。`KIRO_CLIENT_TOKEN` 作为内置 Key 一直有效 |

数据目录（`KIRO_ADMIN_DATA_DIR`）里的内容：
- `accounts.json`：账号、代理、Key 哈希，含凭据
- `models.json`：后台保存的模型列表
- `usage/`：使用日志
- `error-dumps/`：错误日志

备份和注意事项见 [docs/deployment.md](./docs/deployment.md)。

---

## 环境变量

带"启动失败"标记的变量：写了非法值时，进程会直接退出。这样可以避免"以为改了、其实没生效"。其中不少变量连写成空值（例如 `KIRO_STICKY_WAIT_SECONDS=`）都会导致启动失败，不用的话就整行不写。

**基础**

| 变量 | 默认 | 说明 |
|---|---|---|
| `KIRO_CLIENT_TOKEN` | 必填 | 客户端调用 `/v1/*` 的密钥。没设置会启动失败 |
| `PORT` | `8080` | API 端口。也可以通过命令行第一个参数传，环境变量优先 |
| `BIND_HOST` | 空（所有地址） | API 监听的地址 |
| `GIN_MODE` | `release` | 可选 `debug`、`release`、`test` |

**管理模式**

| 变量 | 默认 | 说明 |
|---|---|---|
| `KIRO_ADMIN_DATA_DIR` | 不设置 | 设置后进入管理模式，值是数据目录 |
| `KIRO_ADMIN_SECRET` | 管理模式下必填 | 管理端的 `X-Kiro-Admin-Token`，至少 32 个字符，必须和 `KIRO_CLIENT_TOKEN` 不同。不满足会启动失败 |
| `KIRO_ADMIN_ORIGINS` | 管理模式下必填 | 允许访问后台的来源，多个用逗号分隔。只能是 https，或者 127.0.0.1 / localhost 上的 http。启动失败 |
| `KIRO_ADMIN_BIND` | `127.0.0.1:8081` | 管理端口监听的地址 |
| `KIRO_AUTH_TOKEN` | 空 | 环境变量模式下的账号配置，可以是 JSON 字符串或文件路径。管理模式下只在 `accounts.json` 不存在时导入一次。格式见 [auth_config.json.example](./auth_config.json.example)，字段有 `auth`（`Social` / `IdC`）、`refreshToken`、`clientId`、`clientSecret`、`disabled` |

**请求处理**

| 变量 | 默认 | 说明 |
|---|---|---|
| `KIRO_IDENTITY_PROMPT` | 内置声明 | 留空用内置声明；`off` 关闭；其他值作为自定义声明全文 |
| `KIRO_SYSTEM_SENSITIVE_WORDS` | 内置规则和 5 个内置词 | 留空表示开启；`off` 关闭；其他值是词表文件路径，一行一个词，追加在内置词后面。文件读不到会启动失败 |
| `KIRO_MODELS_FILE` | 不设置 | 模型列表文件，格式同 `config/models.default.json`。内容不合法会启动失败 |
| `MAX_TOOL_DESCRIPTION_LENGTH` | `10000` | 工具描述的最大长度（按字节算，中文一个字约 3 字节） |

**调度和计费**

| 变量 | 默认 | 说明 |
|---|---|---|
| `KIRO_STICKY_WAIT_SECONDS` | `10` | 续接对话时，原账号并发满的情况下最多等多少秒，取 0 到 60 的整数，0 表示不等。值不合法会启动失败 |
| `KIRO_LOCAL_CACHE_DISCOUNT` | 关闭 | 只有写 `true` 才开启：在本地把一部分输入 token 记成缓存读取，并报告给客户端，让下游（比如 NewAPI）按缓存价计费。这不是 Kiro 真实的缓存用量 |
| `KIRO_LOCAL_CACHE_MAX_PERCENT` | `40` | 命中缓存时，最多把百分之几的输入 token 记成缓存读取。值不合法会启动失败 |
| `KIRO_LOCAL_CACHE_FIXED_PERCENT_ON_HIT` | `true` | `true` 表示命中就按固定比例算；`false` 表示按实际匹配的前缀长度算。值不合法会启动失败 |
| `KIRO_LOCAL_CACHE_MIN_UNCACHED_TOKENS` | `0` | 折算后至少保留多少个未缓存的 token。值不合法会启动失败 |

**排查**

| 变量 | 默认 | 说明 |
|---|---|---|
| `KIRO_ERROR_DUMP` | 开启 | 只在管理模式下生效。留空表示开启；`off` 关闭；其他值会启动失败 |
| `LOG_LEVEL` | `info` | 可选 `debug`、`info`、`warn`、`error`。设成 `debug` 会记录完整的请求体和上游请求体 |
| `DEBUG` | 不设置 | 设成 `true` 或 `1` 等同于 `LOG_LEVEL=debug` |
| `LOG_FILE` | 不设置 | 日志文件路径。设置后同时写控制台和文件 |
| `LOG_CONSOLE` | `true` | 只在设置了 `LOG_FILE` 时有用，`false` 表示不再输出到控制台 |
| `LOG_ENABLE_CALLER`、`LOG_CALLER_SKIP` | 关闭、`3` | 日志里附带代码位置 |

日志固定是 JSON 格式，代码不读取 `LOG_FORMAT`（`docker-compose.yml` 里仍传了这个变量，不影响运行）。

---

## 排查问题

**先看错误日志页**：请求出错时，到后台"错误日志"页按时间或 `request_id` 找到对应记录，点开就能看到客户端发了什么、kiro2api 发给 Kiro 的是什么、Kiro 回了什么（包括 `x-amzn-requestid` 等响应头）。主日志里对应的那一行也带着 `error_dump` 字段，可以对上。

**看容器日志**

```bash
docker compose logs -f --tail 200 kiro2api   # 实时查看最近日志
docker compose logs kiro2api | grep '"level":"ERROR"'   # 只看错误
```

**常见情况**

| 现象 | 原因和处理 |
|---|---|
| 容器启动后马上退出 | 先看日志。最常见的原因：缺少 `KIRO_CLIENT_TOKEN`；`KIRO_ADMIN_SECRET` 不足 32 位；`KIRO_ADMIN_ORIGINS` 格式不对；某个可选变量写成了空值或非法值；词表文件或模型文件读不到 |
| 数据目录读写失败 | `data/` 目录要归属 `1001:1001`，权限设为 700 |
| 打开管理后台返回 401 / 403 | 401 表示没有注入 `X-Kiro-Admin-Token`；403 表示浏览器的来源和 `KIRO_ADMIN_ORIGINS` 不一致 |
| `/v1/*` 返回 401 | Key 填错了，或者这个 Key 已经被停用 / 删除 |
| 返回 503 "没有可用的上游账号" | 所有账号都处于停用、冷却、额度用完或凭据失效的状态，到账号池页面查看具体状态 |
| 返回 429 "账号并发已满" | 所有账号的并发都占满了。可以调高并发上限或者增加账号 |
| 返回 400 `model_not_found` | 模型名不在列表里，用 `/v1/models` 查可用的名字，或者到后台检测线上模型 |

排查时不要用 `docker exec kiro2api env` 打印完整的环境变量，那样会把密钥打出来。只看变量名的写法见 [docs/deployment.md](./docs/deployment.md)。

---

## 开发

```bash
go build -o kiro2api main.go   # 编译
go test ./...                  # 运行全部测试
go test -race ./server         # 检查并发问题
go vet ./... && gofmt -l .     # 静态检查和格式检查
```

代码结构：

| 目录 | 职责 |
|---|---|
| `server/` | HTTP 路由、处理器、中间件、管理接口、错误日志 |
| `converter/` | Anthropic、OpenAI 与 CodeWhisperer 之间的格式转换，身份声明、system 清洗 |
| `parser/` | Kiro 二进制 EventStream 解析、工具调用、思考内容 |
| `auth/` | 账号池、凭据刷新、额度查询、API Key、代理、使用日志 |
| `discount/` | 对话前缀识别（账号粘滞）和本地缓存折算 |
| `config/` | 模型列表、思考能力、常量 |
| `utils/` | HTTP 客户端、图片和文档处理、token 估算 |
| `static/admin/` | 管理后台前端，手写 HTML/JS，不需要构建 |

推送 `v*` 格式的 tag 会触发 GitHub Actions，构建双架构镜像并发布到 `ghcr.io/stones-hub/kiro2api`，同时更新 `latest` 和对应的版本号别名。

---

## 相关文档

- [docs/deployment.md](./docs/deployment.md)：服务器部署、Nginx、SSH 隧道、NewAPI、运维
- [docs/account-scheduling.md](./docs/account-scheduling.md)：多账号调度规则
- [docs/identity-prompt.md](./docs/identity-prompt.md)：身份声明
- [docs/system-sanitize.md](./docs/system-sanitize.md)：system 敏感词清洗
- [docs/error-dump.md](./docs/error-dump.md)：错误现场记录
- [docs/UNSUPPORTED_TOOLS.md](./docs/UNSUPPORTED_TOOLS.md)：不支持的工具怎么处理
- `docs/` 下的 CodeWhisperer 协议说明：EventStream 格式和请求规范
