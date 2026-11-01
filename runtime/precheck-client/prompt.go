package precheckclient

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"
)

// NormalizeDraftPrompt is only for drafts, never for a passed or executed prompt.
func NormalizeDraftPrompt(prompt string) string {
	prompt = strings.TrimSpace(prompt)
	for _, prefix := range []string{"请你帮我", "请帮我", "我们希望", "我希望", "我想要", "帮我"} {
		if strings.HasPrefix(prompt, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(prompt, prefix))
		}
	}
	for _, prefix := range []string{"我们用 ", "我们用", "我用 ", "我用"} {
		if strings.HasPrefix(prompt, prefix) {
			return "用" + strings.TrimPrefix(prompt, prefix)
		}
	}
	for _, subject := range []string{"我们", "我"} {
		if strings.HasPrefix(prompt, subject) {
			rest := strings.TrimSpace(strings.TrimPrefix(prompt, subject))
			for _, start := range []string{"每天", "每次", "通常", "经常", "平时", "目前", "现在", "最近", "正在", "在", "需要"} {
				if strings.HasPrefix(rest, start) {
					return rest
				}
			}
		}
	}
	return prompt
}

// CanonicalPrompt is the single byte representation used after the draft
// stage. Prompt files often end with a newline and macOS editors may write
// CRLF; normalizing here prevents the precheck, Trae input and submission
// hashes from referring to different text.
func CanonicalPrompt(prompt string) string {
	prompt = strings.ReplaceAll(prompt, "\r\n", "\n")
	prompt = strings.ReplaceAll(prompt, "\r", "\n")
	return strings.TrimSpace(prompt)
}

var promptMarkup = regexp.MustCompile("(?m)^\\s*(?:#{1,6} |[-*] |[0-9]+[.)] )|```|\\[[^]]+\\]\\([^)]+\\)")
var promptPath = regexp.MustCompile(`(?:/Users/|/home/|[A-Za-z]:\\|(?:\w+/){2,}\w+)`)

func ValidateDraftPrompt(prompt string) error {
	if isPlaceholder(prompt) || len([]rune(prompt)) > 240 {
		return fmt.Errorf("prompt must contain 1 to 240 characters")
	}
	if promptMarkup.MatchString(prompt) || promptPath.MatchString(prompt) {
		return fmt.Errorf("prompt contains Markdown or file paths")
	}
	if strings.Count(prompt, ";")+strings.Count(prompt, "；") > 2 {
		return fmt.Errorf("prompt contains more than two semicolons")
	}
	hard := 0
	for _, word := range []string{"必须", "务必", "严禁", "不得", "一定要"} {
		hard += strings.Count(prompt, word)
	}
	if hard > 3 {
		return fmt.Errorf("prompt contains more than three mandatory phrases")
	}
	return nil
}

func TextSHA256(value string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(value))) }
