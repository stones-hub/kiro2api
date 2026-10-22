package utils

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/macos-cjk.pdf 由 macOS cupsfilter 生成：中文字体同时带 Encoding 字典和
// ToUnicode，原版解析库会把中文提取成乱码。
func testPDFBase64(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/macos-cjk.pdf")
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}

func TestExtractPDFText_CJKToUnicode(t *testing.T) {
	raw, err := os.ReadFile("testdata/macos-cjk.pdf")
	require.NoError(t, err)
	text, pages, err := ExtractPDFText(raw)
	require.NoError(t, err)
	assert.Equal(t, 1, pages)
	assert.Contains(t, text, "--- Page 1 ---")
	assert.Contains(t, text, "Second line: 42 apples")
	assert.Contains(t, text, "测试：随机答案")
	assert.Contains(t, text, "7391")
}

func TestExtractPDFText_Invalid(t *testing.T) {
	_, _, err := ExtractPDFText([]byte("%PDF-1.4 truncated"))
	assert.Error(t, err)
}

func TestDocumentBlockText_PDF(t *testing.T) {
	text := DocumentBlockText(map[string]any{
		"type":   "document",
		"title":  "test.pdf",
		"source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": testPDFBase64(t)},
	})
	assert.True(t, strings.HasPrefix(text, `<document title="test.pdf">`))
	assert.Contains(t, text, "PDF converted to plain text")
	assert.Contains(t, text, "Second line: 42 apples")
	assert.NotContains(t, text, "JVBER") // 不能把 base64 原样发给模型
}

func TestDocumentBlockText_OtherSources(t *testing.T) {
	assert.Contains(t, DocumentBlockText(map[string]any{
		"type": "document", "source": map[string]any{"type": "text", "media_type": "text/plain", "data": "plain body"},
	}), "plain body")
	assert.Contains(t, DocumentBlockText(map[string]any{
		"type": "document", "source": map[string]any{"type": "content", "content": []any{map[string]any{"type": "text", "text": "chunk one"}}},
	}), "chunk one")
	assert.Contains(t, DocumentBlockText(map[string]any{
		"type": "document", "source": map[string]any{"type": "url", "url": "https://example.com/a.pdf"},
	}), "was not fetched")
	assert.Contains(t, DocumentBlockText(map[string]any{
		"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "!!"},
	}), "not valid base64")
}

func TestParseToolResultContent_Document(t *testing.T) {
	text := ParseToolResultContent([]any{
		map[string]any{"type": "text", "text": "PDF file read: test.pdf (16.7KB)"},
		map[string]any{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": testPDFBase64(t)}},
	})
	assert.Contains(t, text, "PDF file read")
	assert.Contains(t, text, "Second line: 42 apples")
	assert.NotContains(t, text, "JVBER")
}

func TestGetMessageContent_DocumentOnly(t *testing.T) {
	content, err := GetMessageContent([]any{map[string]any{"type": "document", "source": map[string]any{"type": "text", "data": "x"}}})
	require.NoError(t, err)
	assert.Equal(t, "[文档]", content)
}
