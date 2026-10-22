package utils

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"

	"github.com/ledongthuc/pdf"
	"kiro2api/logger"
)

// Kiro 的 userInputMessage 只有 images，没有 document 字段。Anthropic 的
// document 块（Claude Code 读 PDF、用户粘贴 PDF/文本文件）在这里转成文本，
// 否则会被静默丢弃，模型只看到 "PDF file read: x.pdf" 这样的提示。

// MaxDocumentTextRunes 限制单个文档转出的文本量，避免一个大 PDF 撑爆上下文
const MaxDocumentTextRunes = 400000

// IsDocumentBlock 判断内容块是否为 Anthropic document 块
func IsDocumentBlock(block map[string]any) bool {
	blockType, _ := block["type"].(string)
	return blockType == "document"
}

// DocumentBlockText 把 document 块转换为发给 Kiro 的文本。
// 无法提取时返回说明性的占位文本，让模型知道有文档但读不到，而不是悄悄丢掉。
func DocumentBlockText(block map[string]any) string {
	title, _ := block["title"].(string)
	context, _ := block["context"].(string)
	source, _ := block["source"].(map[string]any)

	body, note := documentSourceText(source)
	body = strings.TrimSpace(body)

	var b strings.Builder
	b.WriteString("<document")
	if title != "" {
		fmt.Fprintf(&b, " title=%q", title)
	}
	b.WriteString(">\n")
	if context != "" {
		b.WriteString("<document_context>" + context + "</document_context>\n")
	}
	if note != "" {
		b.WriteString("[" + note + "]\n")
	}
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString("</document>")
	return b.String()
}

func documentSourceText(source map[string]any) (body, note string) {
	if source == nil {
		return "", "Document has no source; its content is unavailable."
	}
	sourceType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	switch sourceType {
	case "text":
		data, _ := source["data"].(string)
		return truncateDocumentText(data)
	case "content":
		return truncateDocumentText(contentSourceText(source["content"]))
	case "url":
		url, _ := source["url"].(string)
		return "", fmt.Sprintf("Document referenced by URL %s was not fetched by the proxy; its content is unavailable.", url)
	case "file":
		return "", "Document referenced by file_id is not supported by this proxy; its content is unavailable."
	case "base64":
		data, _ := source["data"].(string)
		raw, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return "", "Document data is not valid base64; its content is unavailable."
		}
		switch {
		case mediaType == "application/pdf" || bytes.HasPrefix(raw, []byte("%PDF-")):
			text, pages, err := cachedPDFText(raw)
			if err != nil {
				logger.Warn("PDF 文本提取失败", logger.Err(err), logger.Int("bytes", len(raw)))
				return "", "PDF text extraction failed (" + err.Error() + "); its content is unavailable. Ask the user for the text or for page images."
			}
			if strings.TrimSpace(text) == "" {
				return "", fmt.Sprintf("PDF with %d page(s) contains no extractable text (it may be scanned images); its content is unavailable. Ask the user for page images instead.", pages)
			}
			body, truncNote := truncateDocumentText(text)
			note := fmt.Sprintf("PDF converted to plain text by the proxy (%d page(s)); layout, images and charts are not included.", pages)
			if truncNote != "" {
				note += " " + truncNote
			}
			return body, note
		case strings.HasPrefix(mediaType, "text/"):
			return truncateDocumentText(string(raw))
		default:
			return "", fmt.Sprintf("Document of type %q is not supported by this proxy; its content is unavailable.", mediaType)
		}
	default:
		return "", fmt.Sprintf("Document source type %q is not supported by this proxy; its content is unavailable.", sourceType)
	}
}

func contentSourceText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, item := range c {
			if m, ok := item.(map[string]any); ok {
				if text, ok := m["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func truncateDocumentText(text string) (string, string) {
	runes := []rune(text)
	if len(runes) <= MaxDocumentTextRunes {
		return text, ""
	}
	return string(runes[:MaxDocumentTextRunes]), fmt.Sprintf("Text truncated to the first %d of %d characters.", MaxDocumentTextRunes, len(runes))
}

type pdfTextResult struct {
	text  string
	pages int
	err   error
}

var (
	pdfTextCacheMu sync.Mutex
	pdfTextCache   = make(map[[32]byte]pdfTextResult)
)

// cachedPDFText 缓存提取结果：会话历史里的 PDF 每次请求都会重新转换，
// 一次请求内也会被多处调用。缓存满了直接清空，保持简单。
func cachedPDFText(raw []byte) (string, int, error) {
	key := sha256.Sum256(raw)
	pdfTextCacheMu.Lock()
	cached, ok := pdfTextCache[key]
	pdfTextCacheMu.Unlock()
	if ok {
		return cached.text, cached.pages, cached.err
	}
	text, pages, err := ExtractPDFText(raw)
	pdfTextCacheMu.Lock()
	if len(pdfTextCache) >= 32 {
		clear(pdfTextCache)
	}
	pdfTextCache[key] = pdfTextResult{text, pages, err}
	pdfTextCacheMu.Unlock()
	return text, pages, err
}

// ExtractPDFText 提取 PDF 每一页的纯文本，页与页之间加页码标记。
// 解析库对畸形文件可能 panic，这里统一转成 error。
func ExtractPDFText(raw []byte) (text string, pages int, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("invalid PDF: %v", r)
		}
	}()
	reader, err := pdf.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", 0, err
	}
	pages = reader.NumPage()
	var b strings.Builder
	for i := 1; i <= pages; i++ {
		page := reader.Page(i)
		if page.V.IsNull() {
			continue
		}
		// 传 nil 让每页使用自己的字体表：字体名（F1 等）只在本页有效
		pageText, err := page.GetPlainText(nil)
		if err != nil {
			return "", pages, fmt.Errorf("page %d: %w", i, err)
		}
		pageText = tidyPDFText(pageText)
		if pageText == "" {
			continue
		}
		fmt.Fprintf(&b, "--- Page %d ---\n%s\n\n", i, pageText)
		if b.Len() > MaxDocumentTextRunes*4 {
			break // 已超过截断上限，剩余页面不再解析
		}
	}
	return strings.TrimSpace(b.String()), pages, nil
}

// tidyPDFText 去掉等宽排版留下的行尾空格和多余空行
func tidyPDFText(text string) string {
	lines := strings.Split(text, "\n")
	out := lines[:0]
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
