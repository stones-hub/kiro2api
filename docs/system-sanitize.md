# 客户端 system 敏感词清洗

## 目标
参考 CLIProxyAPI（CPA）Devin 通道的 `SanitizeDevinSystemPrompt`：客户端 system 提示词里命中指定内容的行直接删除，然后继续请求，不因命中而拒绝。用于去掉 Claude Code 等客户端自带的身份/归属文字。

这不是内容审核：只处理客户端 system，不保证用户消息不含这些词，也不判断内容是否违规。

## 配置
环境变量 `KIRO_SYSTEM_SENSITIVE_WORDS`，启动时读取一次，改动后需重启：
- 未设置或为空：开启，使用内置规则 + 内置 5 个词（默认开启）。
- `off`（忽略大小写和首尾空白）：完全关闭，请求转换与没有此功能时逐字节一致。
- 其他值：词表文件路径。文件一行一个词，空行和 `#` 开头的行忽略；文件里的词**追加**在内置词之后，内置词不能去掉（要去掉只能整体 `off`）。文件读取失败时进程启动失败退出。

内置词：`You are a Claude agent`、`Claude Agent SDK`、`Claude Code`、`claude-code`、`ClaudeCode`。

Docker 部署时把文件放在 `./data/system-sensitive-words.txt`，`.env` 写 `KIRO_SYSTEM_SENSITIVE_WORDS=/app/data/system-sensitive-words.txt`，复用已有的 `./data` 挂载，不改 `docker-compose.yml`。

## 行为
- 范围：只处理客户端 system（`/v1/messages` 的 `system`；`/v1/chat/completions` 的 `system`/`developer` 消息，它们已被转进同一字段）。用户消息、assistant 历史、工具参数、工具结果不处理。两个入口共用 `converter.BuildCodeWhispererRequest`，行为一致。
- 顺序：服务端身份声明（`KIRO_IDENTITY_PROMPT`）先写入且不参与清洗；客户端 system 每个块单独清洗后再追加。
- 每个 system 块：CRLF 统一为 LF，按行检查（检查时用去首尾空白后的行），命中以下任一规则就删除**整行**：
  - CPA 内置规则（区分大小写，与 CPA 一致）：去前导空白后以 `x-anthropic-billing-header:` 开头；以 `You are Claude Code` 开头；包含 `authorized security testing`、`destructive techniques, DoS attacks`、`Claude Code is available as a CLI`、`Fast mode for Claude Code`、`Codex refers to the open-source agentic coding interface`、`- Don’t output ANSI escape codes directly — the CLI renderer applies them.`。
  - 词表（内置 5 个 + 文件追加）：不区分大小写的字面子串匹配，不要求词边界，不当正则执行；词去首尾空白，少于 2 个 Unicode 字符的忽略。
- 保留的行拼回后 TrimSpace。不做 CPA 的零宽混淆：命中词的行已整行删除，剩余内容不会再含词表词，混淆在删行模式下永远不改动内容（用户决定去掉）。
- 清洗后为空的块直接丢弃；客户端 system 全部被删也继续请求。
- 已接受的取舍：整行删除可能连带删掉同一行里的其他要求。

## 验证
- 单元测试：5 个内置词、大小写、整行删除且其他行保留、CRLF、全部删空、`off`、文件追加词、文件不可读启动失败、CPA 内置规则、身份声明不受影响、多块 system 行边界、两个入口一致、用户/历史/工具内容不变。
- 本机候选：真实路由 + 假上游，检查最终发往上游的载荷。
