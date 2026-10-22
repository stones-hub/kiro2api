package converter

import (
	"fmt"
	"strings"

	"kiro2api/logger"
	"kiro2api/types"
	"kiro2api/utils"
)

// 消息内容处理器

// processMessageContent 处理消息内容，提取文本和图片
func processMessageContent(content any) (string, []types.CodeWhispererImage, error) {
	var textParts []string
	var images []types.CodeWhispererImage

	switch v := content.(type) {
	case string:
		// 简单字符串内容
		return v, nil, nil

	case []any:
		// 内容块数组
		for i, item := range v {
			if block, ok := item.(map[string]any); ok {
				contentBlock, err := parseContentBlock(block)
				if err != nil {
					logger.Warn("解析内容块失败，跳过", logger.Err(err), logger.Int("index", i))
					continue // 跳过无法解析的块
				}

				switch contentBlock.Type {
				case "text":
					if contentBlock.Text != nil {
						textParts = append(textParts, *contentBlock.Text)
					} else {
						logger.Warn("文本块的Text字段为nil")
					}
				case "image":
					if cwImage, note := kiroImage(contentBlock.Source); cwImage != nil {
						images = append(images, *cwImage)
					} else {
						textParts = append(textParts, note)
					}
				case "document":
					textParts = append(textParts, "\n"+utils.DocumentBlockText(block)+"\n")
				case "tool_result":
					// 处理工具结果，支持复杂的内容结构
					if contentBlock.Content != nil {
						parsedContent := utils.ParseToolResultContent(contentBlock.Content)
						// 如果内容为空，提供默认值
						if parsedContent == "" {
							parsedContent = "Tool executed successfully"
						}
						textParts = append(textParts, parsedContent)
						images = append(images, extractToolResultImages(contentBlock.Content)...)
					}
				}
			} else {
				logger.Warn("内容块不是map[string]any类型",
					logger.Int("index", i),
					logger.String("actual_type", fmt.Sprintf("%T", item)))
			}
		}

	case []types.ContentBlock:
		// 结构化的内容块数组
		for _, block := range v {
			switch block.Type {
			case "text":
				if block.Text != nil {
					textParts = append(textParts, *block.Text)
				} else {
					logger.Warn("结构化文本块的Text字段为nil")
				}
			case "image":
				if cwImage, note := kiroImage(block.Source); cwImage != nil {
					images = append(images, *cwImage)
				} else {
					textParts = append(textParts, note)
				}
			case "document":
				textParts = append(textParts, "\n"+utils.DocumentBlockText(documentBlockMap(block))+"\n")
			case "tool_result":
				// 处理工具结果，支持复杂的内容结构
				if block.Content != nil {
					parsedContent := utils.ParseToolResultContent(block.Content)
					// 如果内容为空，提供默认值
					if parsedContent == "" {
						parsedContent = "Tool executed successfully"
					}
					textParts = append(textParts, parsedContent)
					images = append(images, extractToolResultImages(block.Content)...)
				}
			}
		}

	default:
		// 不支持的内容类型，返回错误而非fallback
		return "", nil, fmt.Errorf("不支持的内容类型: %T", content)
	}

	result := strings.Join(textParts, "")

	// 保留关键调试信息用于问题定位
	if result == "" && len(images) == 0 {
		logger.Debug("消息内容处理结果为空",
			logger.String("content_type", fmt.Sprintf("%T", content)),
			logger.Int("text_parts_count", len(textParts)),
			logger.Int("images_count", len(images)))
	}

	return result, images, nil
}

// siblingTextOfToolResults 返回用户消息中 tool_result 之外的文本和文档内容。
// 带 tool_result 的消息里，tool_result 本身的文本已放进 Kiro 的 toolResults，
// 但并列的文本块（Claude Code 的用户插话、system-reminder）和文档块
// （Read 读 PDF 时的 document）只能通过 content 发送。
func siblingTextOfToolResults(content any) string {
	var parts []string
	switch v := content.(type) {
	case []any:
		for _, item := range v {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			case "document":
				parts = append(parts, utils.DocumentBlockText(block))
			}
		}
	case []types.ContentBlock:
		for _, block := range v {
			switch block.Type {
			case "text":
				if block.Text != nil && strings.TrimSpace(*block.Text) != "" {
					parts = append(parts, *block.Text)
				}
			case "document":
				parts = append(parts, utils.DocumentBlockText(documentBlockMap(block)))
			}
		}
	}
	return strings.Join(parts, "\n")
}

func siblingTextOfMessages(messages []types.AnthropicRequestMessage) string {
	var parts []string
	for _, msg := range messages {
		if text := siblingTextOfToolResults(msg.Content); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// documentBlockMap 把结构化 document 块还原为 map，供 utils.DocumentBlockText 使用
func documentBlockMap(block types.ContentBlock) map[string]any {
	m := map[string]any{"type": block.Type}
	if block.Source != nil {
		m["source"] = map[string]any{"type": block.Source.Type, "media_type": block.Source.MediaType, "data": block.Source.Data}
	}
	return m
}

// kiroImage 把 Anthropic 图片转换为 Kiro 格式。无效图片返回 nil 和一行说明文字：
// 图片会一直留在会话历史里，若让整条消息或整个请求失败，该会话之后的每次请求都会出错。
func kiroImage(source *types.ImageSource) (*types.CodeWhispererImage, string) {
	err := fmt.Errorf("图片缺少source")
	if source != nil {
		source = utils.CorrectImageMediaType(source)
		err = utils.ValidateImageContent(source)
	}
	if err == nil {
		if cwImage := utils.CreateCodeWhispererImage(source); cwImage != nil {
			return cwImage, ""
		}
		err = fmt.Errorf("不支持的图片格式")
	}
	logger.Warn("图片无效，已跳过", logger.Err(err))
	return nil, fmt.Sprintf("[Image omitted by proxy: %v]", err)
}

// imageOnlyUserContent 是只有图片、没有文字的用户消息的文本内容，Kiro 拒绝空 content
const imageOnlyUserContent = "Please analyze the attached image."

// extractToolResultImages 提取 tool_result content 中的图片（Claude Code 的 Read
// 工具读取图片时即为此结构）。Kiro 的 toolResults 只接受文本，图片需要放到同一条
// userInputMessage 的 images 中。无效图片只记录警告并跳过：它会一直留在会话历史里，
// 若让整个请求失败，该会话之后的每次请求都会失败。
func extractToolResultImages(content any) []types.CodeWhispererImage {
	items, ok := content.([]any)
	if !ok {
		return nil
	}

	var images []types.CodeWhispererImage
	for i, item := range items {
		block, ok := item.(map[string]any)
		if !ok || !utils.IsImageBlock(block) {
			continue
		}
		contentBlock, err := parseContentBlock(block)
		if err != nil {
			logger.Warn("tool_result中的图片无效，跳过", logger.Err(err), logger.Int("index", i))
			continue
		}
		if cwImage, _ := kiroImage(contentBlock.Source); cwImage != nil {
			images = append(images, *cwImage)
		}
	}
	return images
}

// parseContentBlock 解析内容块
func parseContentBlock(block map[string]any) (types.ContentBlock, error) {
	var contentBlock types.ContentBlock

	// 解析类型
	if blockType, ok := block["type"].(string); ok {
		contentBlock.Type = blockType
	} else {
		logger.Error("内容块缺少type字段或type不是字符串",
			logger.String("type_value", fmt.Sprintf("%v", block["type"])),
			logger.String("type_type", fmt.Sprintf("%T", block["type"])))
		return contentBlock, fmt.Errorf("缺少内容块类型")
	}

	// 根据类型解析不同字段
	switch contentBlock.Type {
	case "text":
		if text, ok := block["text"].(string); ok {
			contentBlock.Text = &text
		} else {
			logger.Warn("文本块缺少text字段或不是字符串",
				logger.String("text_value", fmt.Sprintf("%v", block["text"])),
				logger.String("text_type", fmt.Sprintf("%T", block["text"])))
		}

	case "image":
		if source, ok := block["source"].(map[string]any); ok {
			imageSource := &types.ImageSource{}

			if sourceType, ok := source["type"].(string); ok {
				imageSource.Type = sourceType
			}
			if mediaType, ok := source["media_type"].(string); ok {
				imageSource.MediaType = mediaType
			}
			if data, ok := source["data"].(string); ok {
				imageSource.Data = data
			}

			contentBlock.Source = imageSource
		}

	case "image_url":
		// 处理OpenAI格式的图片块，转换为Anthropic格式
		if imageURL, ok := block["image_url"].(map[string]any); ok {
			imageSource, err := utils.ConvertImageURLToImageSource(imageURL)
			if err != nil {
				return contentBlock, fmt.Errorf("转换image_url失败: %v", err)
			}
			// 将类型改为image并设置source
			contentBlock.Type = "image"
			contentBlock.Source = imageSource
		}

	case "tool_result":
		if toolUseId, ok := block["tool_use_id"].(string); ok {
			contentBlock.ToolUseId = &toolUseId
		}
		if content, ok := block["content"]; ok {
			contentBlock.Content = content
		}
		if isError, ok := block["is_error"].(bool); ok {
			contentBlock.IsError = &isError
		}

	case "tool_use":
		if id, ok := block["id"].(string); ok {
			contentBlock.ID = &id
		}
		if name, ok := block["name"].(string); ok {
			contentBlock.Name = &name
		}
		if input, ok := block["input"]; ok {
			contentBlock.Input = &input
		}
	}

	return contentBlock, nil
}
