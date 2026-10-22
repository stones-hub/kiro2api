package converter

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"kiro2api/config"
	"kiro2api/logger"
	"kiro2api/types"
	"kiro2api/utils"

	"github.com/gin-gonic/gin"
)

// systemPromptAssistantPriming is the protocol-level assistant turn used to
// close the synthetic user turn that carries a system prompt. It is only used
// for that priming pair; normal assistant history must always come from the
// client's actual model output.
const systemPromptAssistantPriming = "I will follow these instructions."

// identityPromptAssistantPriming replaces the plain priming whenever an
// identity statement is injected, so the synthetic assistant turn
// acknowledges the statement instead of generic instructions.
const identityPromptAssistantPriming = "Understood. I will follow these instructions, including the identity statement."

// DefaultIdentityPrompt is prepended to the system prompt so the model answers
// identity questions as Claude. Upstream Kiro ships its own hidden system
// prompt that makes models call themselves Kiro; this only lowers the odds of
// such an answer, it cannot rule it out.
const DefaultIdentityPrompt = "You are Claude, an AI assistant developed by Anthropic. When asked about your identity, which model you are, or who created you, answer that you are Claude, developed by Anthropic. Never describe yourself as Kiro, Amazon Q, CodeWhisperer, or any Amazon/AWS product, and never claim to be developed by Amazon or AWS."

// identityPrompt is the statement injected ahead of the client's system
// prompt. Empty means no injection, which keeps request building byte-for-byte
// identical to the behaviour before this feature existed. main applies the
// KIRO_IDENTITY_PROMPT policy once at startup through SetIdentityPrompt, so
// the shipped default is enabled; callers that never configure it (tests,
// future tooling) opt in explicitly.
var identityPrompt string

// SetIdentityPrompt applies a raw KIRO_IDENTITY_PROMPT value: empty or unset
// uses DefaultIdentityPrompt, "off" disables injection, anything else is used
// verbatim as the whole statement. Call it during startup, before serving.
func SetIdentityPrompt(raw string) {
	switch trimmed := strings.TrimSpace(raw); {
	case trimmed == "":
		identityPrompt = DefaultIdentityPrompt
	case strings.EqualFold(trimmed, "off"):
		identityPrompt = ""
	default:
		identityPrompt = raw
	}
}

// systemPromptPriming returns the assistant priming that closes the synthetic
// system turn, acknowledging the identity statement when one is injected.
func systemPromptPriming() string {
	if identityPrompt != "" {
		return identityPromptAssistantPriming
	}
	return systemPromptAssistantPriming
}

// ValidateAssistantResponseEvent 验证助手响应事件
// ConvertToAssistantResponseEvent 转换任意数据为标准的AssistantResponseEvent
// NormalizeAssistantResponseEvent 标准化助手响应事件（填充默认值等）
// normalizeWebLinks 标准化网页链接
// normalizeReferences 标准化引用
// CodeWhisperer格式转换器

// determineChatTriggerType 智能确定聊天触发类型 (SOLID-SRP: 单一责任)
func determineChatTriggerType(anthropicReq types.AnthropicRequest) string {
	// 如果有工具调用，通常是自动触发的
	if len(anthropicReq.Tools) > 0 {
		// 检查tool_choice是否强制要求使用工具
		if anthropicReq.ToolChoice != nil {
			if tc, ok := anthropicReq.ToolChoice.(*types.ToolChoice); ok && tc != nil {
				if tc.Type == "any" || tc.Type == "tool" {
					return "AUTO" // 自动工具调用
				}
			} else if tcMap, ok := anthropicReq.ToolChoice.(map[string]any); ok {
				if tcType, exists := tcMap["type"].(string); exists {
					if tcType == "any" || tcType == "tool" {
						return "AUTO" // 自动工具调用
					}
				}
			}
		}
	}

	// 默认为手动触发
	return "MANUAL"
}

// toolResultContentArray 将 tool_result 的 content 转换为 Kiro 的数组格式。
// 图片块替换为文本占位，图片本身由 processMessageContent 放入 images；文档块转成文本。
func toolResultContentArray(content any) []map[string]any {
	switch c := content.(type) {
	case string:
		return []map[string]any{{"text": c}}
	case []any:
		var contentArray []map[string]any
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if utils.IsImageBlock(m) {
				contentArray = append(contentArray, map[string]any{"text": utils.ToolResultImagePlaceholder})
				continue
			}
			if utils.IsDocumentBlock(m) {
				contentArray = append(contentArray, map[string]any{"text": utils.DocumentBlockText(m)})
				continue
			}
			contentArray = append(contentArray, m)
		}
		return contentArray
	case map[string]any:
		return []map[string]any{c}
	default:
		return []map[string]any{{"text": fmt.Sprintf("%v", c)}}
	}
}

// validateCodeWhispererRequest 验证CodeWhisperer请求的完整性 (SOLID-SRP: 单一责任验证)
func validateCodeWhispererRequest(cwReq *types.CodeWhispererRequest) error {
	// 验证必需字段
	if cwReq.ConversationState.CurrentMessage.UserInputMessage.ModelId == "" {
		return fmt.Errorf("ModelId不能为空")
	}

	if cwReq.ConversationState.ConversationId == "" {
		return fmt.Errorf("ConversationId不能为空")
	}

	// 验证内容完整性 (KISS: 简化内容验证)
	trimmedContent := strings.TrimSpace(cwReq.ConversationState.CurrentMessage.UserInputMessage.Content)
	hasImages := len(cwReq.ConversationState.CurrentMessage.UserInputMessage.Images) > 0
	hasTools := len(cwReq.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools) > 0
	hasToolResults := len(cwReq.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults) > 0

	// 如果有工具结果，允许内容为空（这是工具执行后的反馈请求）
	if hasToolResults {
		logger.Debug("检测到工具结果，允许内容为空",
			logger.String("conversation_id", cwReq.ConversationState.ConversationId),
			logger.Int("tool_results_count", len(cwReq.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults)))
		return nil
	}

	// 如果没有内容但有工具，注入占位内容 (YAGNI: 只在需要时处理)
	if trimmedContent == "" && !hasImages && hasTools {
		placeholder := "执行工具任务"
		cwReq.ConversationState.CurrentMessage.UserInputMessage.Content = placeholder
		logger.Warn("注入占位内容以触发工具调用",
			logger.String("conversation_id", cwReq.ConversationState.ConversationId),
			logger.Int("tools_count", len(cwReq.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools)))
		trimmedContent = placeholder
	}

	// 验证至少有内容或图片
	if trimmedContent == "" && !hasImages {
		return fmt.Errorf("用户消息内容和图片都为空")
	}

	return nil
}

// extractToolResultsFromMessage 从消息内容中提取工具结果
func extractToolResultsFromMessage(content any) []types.ToolResult {
	var toolResults []types.ToolResult

	switch v := content.(type) {
	case []any:
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if blockType, exists := block["type"]; exists {
					if typeStr, ok := blockType.(string); ok && typeStr == "tool_result" {
						toolResult := types.ToolResult{}

						// 提取 tool_use_id
						if toolUseId, ok := block["tool_use_id"].(string); ok {
							toolResult.ToolUseId = kiroToolUseID(toolUseId)
						}

						// 提取 content - 转换为数组格式
						if content, exists := block["content"]; exists {
							toolResult.Content = toolResultContentArray(content)
						}

						// 提取 status (默认为 success)
						toolResult.Status = "success"
						if isError, ok := block["is_error"].(bool); ok && isError {
							toolResult.Status = "error"
							toolResult.IsError = true
						}

						toolResults = append(toolResults, toolResult)

						// logger.Debug("提取到工具结果",
						// 	logger.String("tool_use_id", toolResult.ToolUseId),
						// 	logger.String("status", toolResult.Status),
						// 	logger.Int("content_items", len(toolResult.Content)))
					}
				}
			}
		}
	case []types.ContentBlock:
		for _, block := range v {
			if block.Type == "tool_result" {
				toolResult := types.ToolResult{}

				if block.ToolUseId != nil {
					toolResult.ToolUseId = kiroToolUseID(*block.ToolUseId)
				}

				// 处理 content
				if block.Content != nil {
					toolResult.Content = toolResultContentArray(block.Content)
				}

				// 设置 status
				toolResult.Status = "success"
				if block.IsError != nil && *block.IsError {
					toolResult.Status = "error"
					toolResult.IsError = true
				}

				toolResults = append(toolResults, toolResult)
			}
		}
	}

	return toolResults
}

// BuildCodeWhispererRequest 构建 CodeWhisperer 请求
func BuildCodeWhispererRequest(anthropicReq types.AnthropicRequest, ctx *gin.Context) (types.CodeWhispererRequest, error) {
	// logger.Debug("构建CodeWhisperer请求", logger.String("profile_arn", profileArn))

	cwReq := types.CodeWhispererRequest{}

	// 设置代理相关字段 (基于参考文档的标准配置)
	// 使用稳定的代理延续ID生成器，保持会话连续性 (KISS + DRY原则)
	cwReq.ConversationState.AgentContinuationId = utils.GenerateStableAgentContinuationID(ctx)
	cwReq.ConversationState.AgentTaskType = "vibe" // 固定设置为"vibe"，符合参考文档

	// 智能设置ChatTriggerType (KISS: 简化逻辑但保持准确性)
	cwReq.ConversationState.ChatTriggerType = determineChatTriggerType(anthropicReq)

	// 使用稳定的会话ID生成器，基于客户端信息生成持久化的conversationId
	if ctx != nil {
		cwReq.ConversationState.ConversationId = utils.GenerateStableConversationID(ctx)

		// 调试日志：记录会话ID生成信息
		// clientInfo := utils.ExtractClientInfo(ctx)
		// logger.Debug("生成稳定会话ID",
		// 	logger.String("conversation_id", cwReq.ConversationState.ConversationId),
		// 	logger.String("agent_continuation_id", cwReq.ConversationState.AgentContinuationId),
		// 	logger.String("agent_task_type", cwReq.ConversationState.AgentTaskType),
		// 	logger.String("client_ip", clientInfo["client_ip"]),
		// 	logger.String("user_agent", clientInfo["user_agent"]),
		// 	logger.String("custom_conv_id", clientInfo["custom_conv_id"]),
		// logger.String("custom_agent_cont_id", clientInfo["custom_agent_cont_id"]))
	} else {
		// 向后兼容：如果没有提供context，仍使用UUID
		cwReq.ConversationState.ConversationId = utils.GenerateUUID()
		logger.Debug("使用随机UUID作为会话ID（向后兼容）",
			logger.String("conversation_id", cwReq.ConversationState.ConversationId),
			logger.String("agent_continuation_id", cwReq.ConversationState.AgentContinuationId),
			logger.String("agent_task_type", cwReq.ConversationState.AgentTaskType))
	}

	// 处理最后一条消息，包括图片
	if len(anthropicReq.Messages) == 0 {
		return cwReq, fmt.Errorf("消息列表为空")
	}

	lastMessage := anthropicReq.Messages[len(anthropicReq.Messages)-1]

	// 调试：记录原始消息内容
	// logger.Debug("处理用户消息",
	// 	logger.String("role", lastMessage.Role),
	// 	logger.String("content_type", fmt.Sprintf("%T", lastMessage.Content)))

	textContent, images, err := processMessageContent(lastMessage.Content)
	if err != nil {
		return cwReq, fmt.Errorf("处理消息内容失败: %v", err)
	}

	cwReq.ConversationState.CurrentMessage.UserInputMessage.Content = textContent
	// 确保Images字段始终是数组，即使为空
	if len(images) > 0 {
		cwReq.ConversationState.CurrentMessage.UserInputMessage.Images = images
	} else {
		cwReq.ConversationState.CurrentMessage.UserInputMessage.Images = []types.CodeWhispererImage{}
	}

	// 新增：检查并处理 ToolResults
	if lastMessage.Role == "user" {
		toolResults := extractToolResultsFromMessage(lastMessage.Content)
		if len(toolResults) > 0 {
			cwReq.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults = toolResults

			logger.Debug("已添加工具结果到请求",
				logger.Int("tool_results_count", len(toolResults)),
				logger.String("conversation_id", cwReq.ConversationState.ConversationId))

			// tool_result 的文本已在 toolResults 中，content 只保留同一消息里并列的
			// 文本/文档块（用户插话、system-reminder、Read PDF 的文档），不能整个清空
			cwReq.ConversationState.CurrentMessage.UserInputMessage.Content = siblingTextOfToolResults(lastMessage.Content)
		}
	}
	currentInput := &cwReq.ConversationState.CurrentMessage.UserInputMessage
	if strings.TrimSpace(currentInput.Content) == "" && len(currentInput.Images) > 0 && len(currentInput.UserInputMessageContext.ToolResults) == 0 {
		currentInput.Content = imageOnlyUserContent
	}

	// 检查模型映射是否存在，如果不存在则返回错误
	model, supported := config.ResolveModel(anthropicReq.Model)
	if !supported {
		logger.Warn("模型映射不存在",
			logger.String("requested_model", anthropicReq.Model),
			logger.String("request_id", cwReq.ConversationState.AgentContinuationId))

		// 返回模型未找到错误，使用已生成的AgentContinuationId
		return cwReq, types.NewModelNotFoundErrorType(anthropicReq.Model, cwReq.ConversationState.AgentContinuationId)
	}
	modelId := model.ID
	cwReq.ConversationState.CurrentMessage.UserInputMessage.ModelId = modelId
	cwReq.ConversationState.CurrentMessage.UserInputMessage.Origin = "AI_EDITOR" // v0.4兼容性：固定使用AI_EDITOR

	// 处理 tools 信息 - 根据req.json实际结构优化工具转换
	if len(anthropicReq.Tools) > 0 {
		// logger.Debug("开始处理工具配置",
		// 	logger.Int("tools_count", len(anthropicReq.Tools)),
		// 	logger.String("conversation_id", cwReq.ConversationState.ConversationId))

		var tools []types.CodeWhispererTool
		for i, tool := range anthropicReq.Tools {
			// 验证工具定义的完整性 (SOLID-SRP: 单一责任验证)
			if tool.Name == "" {
				logger.Warn("跳过无名称的工具", logger.Int("tool_index", i))
				continue
			}

			// 过滤不支持的工具：web_search (静默过滤，不发送到上游)
			if tool.Name == "web_search" || tool.Name == "websearch" {
				continue
			}

			// logger.Debug("转换工具定义",
			// 	logger.Int("tool_index", i),
			// 	logger.String("tool_name", tool.Name),
			// logger.String("tool_description", tool.Description)
			// )

			// 根据req.json的实际结构，确保JSON Schema完整性
			cwTool := types.CodeWhispererTool{}
			cwTool.ToolSpecification.Name = tool.Name

			// 限制 description 长度为 10000 字符
			if len(tool.Description) > config.MaxToolDescriptionLength {
				cwTool.ToolSpecification.Description = tool.Description[:config.MaxToolDescriptionLength]
				logger.Debug("工具描述超长已截断",
					logger.String("tool_name", tool.Name),
					logger.Int("original_length", len(tool.Description)),
					logger.Int("max_length", config.MaxToolDescriptionLength))
			} else {
				cwTool.ToolSpecification.Description = tool.Description
			}

			// 直接使用原始的InputSchema，避免过度处理 (恢复v0.4兼容性)
			cwTool.ToolSpecification.InputSchema = types.InputSchema{
				Json: tool.InputSchema,
			}
			tools = append(tools, cwTool)
		}

		// 工具配置放在 UserInputMessageContext.Tools 中 (符合req.json结构)
		cwReq.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.Tools = tools
	}

	// 构建历史消息
	// 身份声明开启时必须构建 history，即使没有 system、只有一条消息、没有工具
	if identityPrompt != "" || len(anthropicReq.System) > 0 || len(anthropicReq.Messages) > 1 || len(anthropicReq.Tools) > 0 {
		var history []any

		// 构建综合系统提示
		var systemContentBuilder strings.Builder

		// 身份声明排在最前面，客户端原有 system 原样保留在后面
		if identityPrompt != "" {
			systemContentBuilder.WriteString(identityPrompt)
			systemContentBuilder.WriteString("\n")
		}

		// 添加原有的 system 消息
		if len(anthropicReq.System) > 0 {
			for _, sysMsg := range anthropicReq.System {
				content, err := utils.GetMessageContent(sysMsg)
				if err == nil {
					systemContentBuilder.WriteString(content)
					systemContentBuilder.WriteString("\n")
				}
			}
		}

		// Kiro carries system instructions as the first user history turn. The
		// protocol expects that synthetic turn to be closed by an assistant
		// priming response, but it must be clearly distinguishable from a real
		// conversation answer. Never use a value such as "OK" here: the model
		// can replay it as if it were an actual previous answer.
		if systemContentBuilder.Len() > 0 {
			userMsg := types.HistoryUserMessage{}
			userMsg.UserInputMessage.Content = strings.TrimSpace(systemContentBuilder.String())
			userMsg.UserInputMessage.ModelId = modelId
			userMsg.UserInputMessage.Origin = "AI_EDITOR" // v0.4兼容性：固定使用AI_EDITOR
			history = append(history, userMsg)

			assistantMsg := types.HistoryAssistantMessage{}
			assistantMsg.AssistantResponseMessage.Content = systemPromptPriming()
			history = append(history, assistantMsg)
		}

		// 然后处理常规消息历史 (修复配对逻辑：合并连续user消息，然后与assistant配对)
		// 关键修复：收集连续的user消息并合并，遇到assistant时配对添加
		var userMessagesBuffer []types.AnthropicRequestMessage // 累积连续的user消息

		// 决定历史消息的循环边界
		// 关键修复：如果最后一条消息是assistant，应该将它加入历史（与前面的user配对）
		// 如果最后一条是user，它作为currentMessage，不加入历史
		historyEndIndex := len(anthropicReq.Messages) - 1
		if lastMessage.Role == "assistant" {
			historyEndIndex = len(anthropicReq.Messages) // 包含最后一条assistant
		}

		for i := 0; i < historyEndIndex; i++ {
			msg := anthropicReq.Messages[i]

			if msg.Role == "user" {
				// 收集user消息到缓冲区
				userMessagesBuffer = append(userMessagesBuffer, msg)
				continue
			}
			if msg.Role == "assistant" {
				// 遇到assistant，只有当有对应的user消息时才处理（忽略孤立assistant）
				if len(userMessagesBuffer) > 0 {
					// 合并所有累积的user消息
					mergedUserMsg := types.HistoryUserMessage{}
					var contentParts []string
					var allImages []types.CodeWhispererImage
					var allToolResults []types.ToolResult

					for _, userMsg := range userMessagesBuffer {
						// 处理每个user消息的内容和图片
						messageContent, messageImages, err := processMessageContent(userMsg.Content)
						if err == nil {
							if messageContent != "" {
								contentParts = append(contentParts, messageContent)
							}
							allImages = append(allImages, messageImages...)
						}

						// 收集工具结果
						toolResults := extractToolResultsFromMessage(userMsg.Content)
						if len(toolResults) > 0 {
							allToolResults = append(allToolResults, toolResults...)
						}
					}

					// 设置合并后的内容
					mergedUserMsg.UserInputMessage.Content = strings.Join(contentParts, "\n")
					if len(allImages) > 0 {
						mergedUserMsg.UserInputMessage.Images = allImages
					}
					if len(allToolResults) > 0 {
						mergedUserMsg.UserInputMessage.UserInputMessageContext.ToolResults = allToolResults
						// 与当前消息相同：只保留 tool_result 之外的文本/文档块
						mergedUserMsg.UserInputMessage.Content = siblingTextOfMessages(userMessagesBuffer)
						// logger.Debug("历史用户消息包含工具结果",
						// 	logger.Int("merged_messages", len(userMessagesBuffer)),
						// 	logger.Int("tool_results_count", len(allToolResults)))
					}

					if mergedUserMsg.UserInputMessage.Content == "" && len(allImages) > 0 && len(allToolResults) == 0 {
						mergedUserMsg.UserInputMessage.Content = imageOnlyUserContent
					}

					mergedUserMsg.UserInputMessage.ModelId = modelId
					mergedUserMsg.UserInputMessage.Origin = "AI_EDITOR"
					history = append(history, mergedUserMsg)

					// 清空缓冲区
					userMessagesBuffer = nil

					// 添加assistant消息（只在有配对的user时添加）
					assistantMsg := types.HistoryAssistantMessage{}
					reasoning, reasoningErr := extractReasoningHistory(msg.Content)
					if reasoningErr != nil {
						return cwReq, reasoningErr
					}
					assistantMsg.AssistantResponseMessage.ReasoningContent = reasoning
					assistantContent, err := utils.GetMessageContent(msg.Content)
					if err == nil {
						assistantMsg.AssistantResponseMessage.Content = assistantContent
					} else {
						assistantMsg.AssistantResponseMessage.Content = ""
					}

					// 提取助手消息中的工具调用
					toolUses := extractToolUsesFromMessage(msg.Content)
					if len(toolUses) > 0 {
						assistantMsg.AssistantResponseMessage.ToolUses = toolUses
					} else {
						assistantMsg.AssistantResponseMessage.ToolUses = nil
					}

					// An assistant turn with no text, no tool use and no signed
					// reasoning is not a real response. Dropping it prevents empty
					// protocol artifacts from becoming examples the model can copy.
					if !hasAssistantHistoryPayload(assistantMsg) {
						logger.Debug("忽略没有有效内容的历史assistant消息")
						continue
					}

					history = append(history, assistantMsg)
				}
				// 如果buffer为空，孤立的assistant消息被忽略（不添加到history）
			}
		}

		// 处理结尾的连续user消息。它们是有效的历史输入，即使没有对应
		// 的assistant回复；不能凭空补一条模型回复，否则会污染上下文。
		if len(userMessagesBuffer) > 0 {
			// 合并所有孤立的user消息
			mergedOrphanUserMsg := types.HistoryUserMessage{}
			var contentParts []string
			var allImages []types.CodeWhispererImage
			var allToolResults []types.ToolResult

			for _, userMsg := range userMessagesBuffer {
				messageContent, messageImages, err := processMessageContent(userMsg.Content)
				if err == nil {
					if messageContent != "" {
						contentParts = append(contentParts, messageContent)
					}
					allImages = append(allImages, messageImages...)
				}

				toolResults := extractToolResultsFromMessage(userMsg.Content)
				if len(toolResults) > 0 {
					allToolResults = append(allToolResults, toolResults...)
				}
			}

			mergedOrphanUserMsg.UserInputMessage.Content = strings.Join(contentParts, "\n")
			if len(allImages) > 0 {
				mergedOrphanUserMsg.UserInputMessage.Images = allImages
			}
			if len(allToolResults) > 0 {
				mergedOrphanUserMsg.UserInputMessage.UserInputMessageContext.ToolResults = allToolResults
				mergedOrphanUserMsg.UserInputMessage.Content = siblingTextOfMessages(userMessagesBuffer)
			}

			if mergedOrphanUserMsg.UserInputMessage.Content == "" && len(allImages) > 0 && len(allToolResults) == 0 {
				mergedOrphanUserMsg.UserInputMessage.Content = imageOnlyUserContent
			}

			mergedOrphanUserMsg.UserInputMessage.ModelId = modelId
			mergedOrphanUserMsg.UserInputMessage.Origin = "AI_EDITOR"
			history = append(history, mergedOrphanUserMsg)

			logger.Debug("历史消息末尾存在连续user消息，保留为用户历史，不伪造assistant",
				logger.Int("orphan_messages", len(userMessagesBuffer)))
		}

		cwReq.ConversationState.History = history
	}

	// 最终验证请求完整性 (KISS: 简化验证逻辑)
	if err := validateCodeWhispererRequest(&cwReq); err != nil {
		return cwReq, fmt.Errorf("请求验证失败: %v", err)
	}

	return cwReq, nil
}

// hasAssistantHistoryPayload reports whether an assistant history entry
// contains something the model actually produced. Tool-only turns are valid,
// as are signed/redacted reasoning turns with no visible text.
func hasAssistantHistoryPayload(msg types.HistoryAssistantMessage) bool {
	response := msg.AssistantResponseMessage
	return strings.TrimSpace(response.Content) != "" ||
		len(response.ToolUses) > 0 ||
		response.ReasoningContent != nil
}

// kiroToolUseID maps a client tool ID onto Kiro's ^[a-zA-Z0-9_-]+$ pattern.
// Valid IDs pass through unchanged. Invalid ones are rewritten deterministically,
// so a tool_use and its tool_result still pair up, and a hash suffix keeps
// IDs that differ only in illegal characters distinct.
func kiroToolUseID(id string) string {
	allowed := func(c byte) bool {
		return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
	}
	valid := true
	for i := 0; i < len(id) && valid; i++ {
		valid = allowed(id[i])
	}
	if valid {
		return id
	}
	var b strings.Builder
	for i := 0; i < len(id) && b.Len() < 64; i++ {
		if allowed(id[i]) {
			b.WriteByte(id[i])
		} else {
			b.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(id))
	return b.String() + "_" + hex.EncodeToString(sum[:6])
}

// extractToolUsesFromMessage 从助手消息内容中提取工具调用
func extractToolUsesFromMessage(content any) []types.ToolUseEntry {
	var toolUses []types.ToolUseEntry

	switch v := content.(type) {
	case []any:
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if blockType, exists := block["type"]; exists {
					if typeStr, ok := blockType.(string); ok && typeStr == "tool_use" {
						toolUse := types.ToolUseEntry{}

						// 提取 id 作为 ToolUseId
						if id, ok := block["id"].(string); ok {
							toolUse.ToolUseId = kiroToolUseID(id)
						}

						// 提取 name
						if name, ok := block["name"].(string); ok {
							toolUse.Name = name
						}

						// 过滤不支持的工具：web_search (静默过滤)
						if toolUse.Name == "web_search" || toolUse.Name == "websearch" {
							continue
						}

						// 提取 input
						if input, ok := block["input"].(map[string]any); ok {
							toolUse.Input = input
						} else {
							// 如果 input 不是 map 或不存在，设置为空对象
							toolUse.Input = map[string]any{}
						}

						toolUses = append(toolUses, toolUse)

						// logger.Debug("提取到历史工具调用", logger.String("tool_id", toolUse.ToolUseId), logger.String("tool_name", toolUse.Name))
					}
				}
			}
		}
	case []types.ContentBlock:
		for _, block := range v {
			if block.Type == "tool_use" {
				toolUse := types.ToolUseEntry{}

				if block.ID != nil {
					toolUse.ToolUseId = kiroToolUseID(*block.ID)
				}

				if block.Name != nil {
					toolUse.Name = *block.Name
				}

				// 过滤不支持的工具：web_search (静默过滤)
				if toolUse.Name == "web_search" || toolUse.Name == "websearch" {
					continue
				}

				if block.Input != nil {
					switch inp := (*block.Input).(type) {
					case map[string]any:
						toolUse.Input = inp
					default:
						toolUse.Input = map[string]any{
							"value": inp,
						}
					}
				} else {
					toolUse.Input = map[string]any{}
				}

				toolUses = append(toolUses, toolUse)
			}
		}
	case string:
		// 如果是纯文本，不包含工具调用
		return nil
	}

	return toolUses
}
