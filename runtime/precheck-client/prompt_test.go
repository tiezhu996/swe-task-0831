package precheckclient

import (
	"strings"
	"testing"
)

func TestDraftPrompt(t *testing.T) {
	if got := NormalizeDraftPrompt("我们用工具定期汇总报表"); got != "用工具定期汇总报表" {
		t.Fatal(got)
	}
	if got := NormalizeDraftPrompt("团队希望定期汇总报表"); got != "团队希望定期汇总报表" {
		t.Fatal(got)
	}
	for input, expected := range map[string]string{
		"我们每天用工具汇总报表": "每天用工具汇总报表",
		"我现在需要批量处理报表": "现在需要批量处理报表",
		"我们的报表格式已经固定": "我们的报表格式已经固定",
	} {
		if got := NormalizeDraftPrompt(input); got != expected {
			t.Fatalf("normalization=%q expected=%q", got, expected)
		}
	}
	for _, bad := range []string{"", strings.Repeat("字", 241), "# 需求", "修改 /Users/example/foo", "a；b；c；d", "必须必须必须必须"} {
		if ValidateDraftPrompt(bad) == nil {
			t.Fatal("invalid prompt accepted")
		}
	}
	if err := ValidateDraftPrompt("定期汇总多份报表，遇到无效输入时容易误发不完整的结果，希望能明确区分并保留可重试信息。"); err != nil {
		t.Fatal(err)
	}
}
