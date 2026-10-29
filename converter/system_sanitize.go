package converter

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"kiro2api/logger"
)

// DefaultSystemSensitiveWords are always active while sanitizing is enabled.
// A KIRO_SYSTEM_SENSITIVE_WORDS file can only append to them.
var DefaultSystemSensitiveWords = []string{
	"You are a Claude agent",
	"Claude Agent SDK",
	"Claude Code",
	"claude-code",
	"ClaudeCode",
}

// systemLineContains are the case-sensitive substrings from CPA's
// SanitizeDevinSystemPrompt; a client system line containing one is dropped.
var systemLineContains = []string{
	"authorized security testing",
	"destructive techniques, DoS attacks",
	"Claude Code is available as a CLI",
	"Fast mode for Claude Code",
	"Codex refers to the open-source agentic coding interface",
	"- Don’t output ANSI escape codes directly — the CLI renderer applies them.",
}

// systemSanitizer is the matcher for configured sensitive words. Nil means
// sanitizing is off, which keeps request building byte-for-byte identical to
// the behaviour before this feature existed. main applies the
// KIRO_SYSTEM_SENSITIVE_WORDS policy once at startup through
// SetSystemSensitiveWords, so the shipped default is enabled.
var systemSanitizer *regexp.Regexp

// SetSystemSensitiveWords applies a raw KIRO_SYSTEM_SENSITIVE_WORDS value:
// empty or unset uses the built-in rules and words, "off" disables
// sanitizing, anything else is a word-list file whose words are appended to
// the built-in ones. Call it during startup, before serving.
func SetSystemSensitiveWords(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if strings.EqualFold(trimmed, "off") {
		systemSanitizer = nil
		logger.Info("客户端 system 敏感词清洗已关闭")
		return nil
	}

	words := append([]string(nil), DefaultSystemSensitiveWords...)
	if trimmed != "" {
		data, err := os.ReadFile(trimmed)
		if err != nil {
			return fmt.Errorf("读取 KIRO_SYSTEM_SENSITIVE_WORDS 词表文件失败: %w", err)
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			words = append(words, line)
		}
	}

	matcher, count := buildSensitiveWordMatcher(words)
	systemSanitizer = matcher
	logger.Info("客户端 system 敏感词清洗已开启", logger.Int("words", count))
	return nil
}

// buildSensitiveWordMatcher mirrors CPA's cloak matcher: words are trimmed,
// those shorter than two runes are dropped, and the rest are matched
// case-insensitively as literals.
func buildSensitiveWordMatcher(words []string) (*regexp.Regexp, int) {
	valid := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.TrimSpace(w)
		if utf8.RuneCountInString(w) >= 2 {
			valid = append(valid, w)
		}
	}
	if len(valid) == 0 {
		return nil, 0
	}
	escaped := make([]string, len(valid))
	for i, w := range valid {
		escaped[i] = regexp.QuoteMeta(w)
	}
	return regexp.MustCompile("(?i)" + strings.Join(escaped, "|")), len(valid)
}

// sanitizeSystemBlock cleans one client system block. keep is false when the
// block is empty after sanitizing and should be skipped entirely. With
// sanitizing off the content is returned untouched and always kept.
func sanitizeSystemBlock(content string) (string, bool) {
	if systemSanitizer == nil {
		return content, true
	}
	cleaned := sanitizeSystemText(content, systemSanitizer)
	return cleaned, cleaned != ""
}

// sanitizeSystemText drops every line that hits a CPA built-in rule or a
// sensitive word, then trims what is left.
func sanitizeSystemText(text string, matcher *regexp.Regexp) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if !shouldDropSystemLine(strings.TrimSpace(line), matcher) {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func shouldDropSystemLine(trimmed string, matcher *regexp.Regexp) bool {
	if strings.HasPrefix(strings.TrimLeftFunc(trimmed, unicode.IsSpace), "x-anthropic-billing-header:") ||
		strings.HasPrefix(trimmed, "You are Claude Code") {
		return true
	}
	for _, s := range systemLineContains {
		if strings.Contains(trimmed, s) {
			return true
		}
	}
	return matcher != nil && matcher.MatchString(trimmed)
}
