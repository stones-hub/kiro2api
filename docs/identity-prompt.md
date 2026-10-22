# 身份声明注入

## 目标
上游 Kiro 自带隐藏系统提示，用户问"你是什么模型"时模型会自称 Kiro。代理在每个请求里注入身份声明，让所有模型统一回答"我是 Claude，由 Anthropic 开发"，不提及 Kiro。只能压低概率，无法 100% 保证。

## 行为
- 环境变量 `KIRO_IDENTITY_PROMPT`：
  - 未设置或为空：使用内置英文声明（默认开启）。
  - `off`：关闭，行为与改动前完全一致。
  - 其他值：作为自定义声明全文。
- 开启时：
  - 声明拼在客户端 system 内容最前面，客户端原有 system 原样保留在后面。
  - 客户端没传 system、只有一条消息、无工具时，也构建 history 注入声明。
  - 合成 system 轮之后的 assistant priming 改为 `Understood. I will follow these instructions, including the identity statement.`；关闭时仍为 `I will follow these instructions.`。
- 对所有模型生效（包括 deepseek、glm、gpt、minimax、qwen、auto）。这是用户的明确决定：非 Claude 模型也会自称 Claude。
- `/v1/messages` 与 `/v1/chat/completions` 共用同一转换逻辑，都会生效。
- 实现：`converter.SetIdentityPrompt` 由 `main.go` 启动时调用；包级零值为关闭，所以不经过 `main.go` 的入口（测试等）默认不注入。

## 代价
每个请求多一段约 60 token 的提示词。

## 验证
- 单元测试：未配置（默认声明）、`off`、自定义值；有 system、无 system 单条消息两种请求结构。
- 本机候选：用假凭证无法连上游，只能验证启动和转换结果；真实效果需部署后用"你是什么模型""who made you"实测。
