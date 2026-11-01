package precheckclient

import (
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type RubricItem struct {
	ID   int    `yaml:"id" json:"id"`
	Type string `yaml:"type" json:"type"`
	Text string `yaml:"text" json:"text"`
}
type rubricDocument struct {
	Rubrics []RubricItem `yaml:"rubrics"`
}

func ParseRubrics(text string) ([]RubricItem, error) {
	var doc rubricDocument
	decoder := yaml.NewDecoder(strings.NewReader(text))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return nil, fmt.Errorf("Rubric 必须是 rubrics 列表的有效 YAML")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("Rubric 只允许一个 YAML 文档")
	}
	if len(doc.Rubrics) < 5 {
		return nil, fmt.Errorf("Rubric 至少 5 条")
	}
	hasF2P := false
	for i, r := range doc.Rubrics {
		if r.ID != i+1 || (r.Type != "f2p" && r.Type != "p2p") || isPlaceholder(r.Text) {
			return nil, fmt.Errorf("Rubric 编号必须连续、type 仅 f2p/p2p，且每条有可观察条件")
		}
		if r.Type == "f2p" {
			hasF2P = true
		}
		if strings.TrimSpace(r.Text) == "功能正常" || strings.TrimSpace(r.Text) == "覆盖相关场景" {
			return nil, fmt.Errorf("Rubric 条件不可判定")
		}
	}
	if !hasF2P {
		return nil, fmt.Errorf("Rubric 至少 1 条 f2p")
	}
	return doc.Rubrics, nil
}

type FieldReviewItem struct {
	Key      string   `json:"field_key"`
	Label    string   `json:"label"`
	Status   string   `json:"status"`
	Rules    []string `json:"rules"`
	Problems []string `json:"problems"`
}
type FieldReviewReport struct {
	SchemaVersion int               `json:"schema_version"`
	CheckedAt     string            `json:"checked_at"`
	PlanSHA256    string            `json:"plan_sha256"`
	Passed        bool              `json:"passed"`
	Fields        []FieldReviewItem `json:"fields"`
}

func FieldRules(key string) []string {
	rules := map[string][]string{
		"title":                    {"概括本题具体行为，不能是占位符"},
		"requirement_prompt":       {"与预检冻结原文一致，不重新润色；不暴露 Issue 来源、凭据或本机路径"},
		"repo_url":                 {"填写原始 GitHub 仓库 URL，不得使用 fork 替换"},
		"commit_ref":               {"单独填写固定原始基线 commit 或版本，不能同时拼接两者"},
		"commit_url":               {"必须是与原仓库不同的 GitHub fork commit URL，不能是基线、分支或 PR"},
		"task_type":                {"使用当前平台枚举，按需求与基线能力判断，不能按产物好坏改类型"},
		"authenticity_note":        {"说明实际场景、基线行为与跨模块难度，不以模型产物倒推题目，不泄露 Issue 来源"},
		"involved_modules":         {"依据原始源码填写实际模块，可写仓库相对路径，不能填写本机绝对路径或泛称"},
		"verify_rubric":            {"有效 rubrics YAML；id 从 1 连续，至少 5 条，type 仅 f2p/p2p，至少 1 条 f2p", "每条明确条件和可观察结果；历史缺失时基于冻结题面和原始基线补建，在评价前冻结，不针对产物新增标准"},
		"artifact_result":          {"逐条覆盖冻结 Rubric；严格 N 通过 或 N 未通过 原因，未通过必须有可复核原因"},
		"question_type":            {"与日志有效轮数一致；少于 100 且效果差必须有实质实现缺陷证据；100 轮枚举不覆盖则阻断"},
		"effective_rounds":         {"只使用同一 Session 唯一 PlanItemHandler 事件数，不估计，不能用持续时间代替"},
		"requirement_completed":    {"按全部 Rubric 结论填写当前平台状态，失败项与完成状态不能冲突"},
		"x_is_more_than_two_hours": {"据真实开始及完成时间填写，不能把上传时间、准备时间或空等待算进去"},
		"trae_session_id":          {"完整原串核对目标工作区与真实日志，不回显，不填写短 ID"},
		"seed_model":               {"根据目标 Session 的实际模型和版本，不根据页面默认值猜测"},
		"trae_session_id_2":        {"按当前收集要求留空"}, "seed_rounds": {"按当前收集要求留空"},
		"x_artifact_shot": {"真实终端截图与验收脚本、日志、退出码一致，不能生成仿终端图"},
		"x_trae_trace":    {"本题 Trae 原生 Markdown，只有一次人工题面，脱敏附件及回执哈希一致"},
	}
	if rule, ok := rules[key]; ok {
		return rule
	}
	return []string{"遵守实时平台必填、类型、长度、日期与枚举规则，内容需有实际来源"}
}

var sourceLeak = regexp.MustCompile(`(?i)https?://github\.com/[^\s]+/(issues|pull)/|\bissue\s*#\s*\d+|参考\s*issue|/Users/|/home/|[A-Za-z]:\\|swe_qa_session|swe_qa_csrf|[0-9a-f]{16}:[0-9a-f]{32}_`)
var resultLine = regexp.MustCompile(`^([1-9][0-9]*) (通过|未通过)(?: (.+))?$`)
var commitLink = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/commit/([0-9a-fA-F]{40})$`)

func validCommitLink(raw, repo, baseline string) bool {
	m := commitLink.FindStringSubmatch(raw)
	if m == nil {
		return false
	}
	u, err := url.Parse(repo)
	if err != nil {
		return false
	}
	return !strings.EqualFold(strings.Trim(u.Path, "/"), m[1]+"/"+m[2]) && !strings.EqualFold(m[3], baseline)
}

func ReviewSubmissionFields(plan SubmissionPlan, schema FormSchema) FieldReviewReport {
	report := FieldReviewReport{SchemaVersion: 1, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), PlanSHA256: PlanSHA256(plan), Passed: true, Fields: []FieldReviewItem{}}
	data, _ := json.Marshal(plan.Payload)
	var normalized map[string]any
	_ = json.Unmarshal(data, &normalized)
	plan.Payload = normalized
	rubrics, rubricErr := ParseRubrics(fmt.Sprint(plan.Payload["verify_rubric"]))
	results := strings.Split(strings.TrimSpace(fmt.Sprint(plan.Payload["artifact_result"])), "\n")
	failures, allResults := 0, rubricErr == nil && len(results) == len(rubrics)
	if allResults {
		for i, line := range results {
			m := resultLine.FindStringSubmatch(line)
			if m == nil {
				allResults = false
				break
			}
			n, _ := strconv.Atoi(m[1])
			if n != i+1 || (m[2] == "未通过" && (len(m) < 4 || strings.TrimSpace(m[3]) == "")) {
				allResults = false
				break
			}
			if m[2] == "未通过" {
				failures++
			}
		}
	}
	for _, field := range schema.Fields {
		item := FieldReviewItem{Key: field.Key, Label: field.Label, Status: "passed", Rules: FieldRules(field.Key), Problems: []string{}}
		block := func(reason string) {
			item.Status = "blocked"
			item.Problems = append(item.Problems, reason)
			report.Passed = false
		}
		value, present := plan.Payload[field.Key]
		text, _ := value.(string)
		required := field.Required || field.Type == "precheck_ref" || (field.RequiredWhen.Field != "" && contains(field.RequiredWhen.Values, fmt.Sprint(plan.Payload[field.RequiredWhen.Field])))
		if field.Auto {
			if field.Key == "requirement_prompt" && sourceLeak.MatchString(plan.ExecutionPrompt) {
				block("冻结题面含 Issue 来源、本机路径或敏感标识")
			}
			item.Status = "review_required"
			if len(item.Problems) > 0 {
				item.Status = "blocked"
			}
			report.Fields = append(report.Fields, item)
			continue
		}
		if field.Type == "attachment" || field.Type == "file" {
			if required && plan.Attachments[field.Key] == "" {
				block("缺少材料")
			}
			report.Fields = append(report.Fields, item)
			continue
		}
		if required && (!present || value == nil || isPlaceholder(fmt.Sprint(value))) {
			block("必填字段缺失或占位")
		}
		if present && value != nil && fmt.Sprint(value) != "" {
			if err := validateOneField(field, value); err != nil {
				block(err.Error())
			}
		}
		if field.Key != "trae_session_id" && field.Key != "trae_session_id_2" && sourceLeak.MatchString(text) {
			block("含 Issue 来源、本机路径或敏感标识")
		}
		switch field.Key {
		case "verify_rubric":
			if rubricErr != nil {
				block(rubricErr.Error())
			} else {
				item.Status = "review_required"
			}
		case "artifact_result":
			if !allResults {
				block("结果必须逐条匹配 Rubric，使用 N 通过 / N 未通过 原因")
			}
		case "commit_url":
			if !validCommitLink(text, fmt.Sprint(plan.Payload["repo_url"]), fmt.Sprint(plan.Payload["commit_ref"])) {
				block("不是不同于原仓库及基线的完整 GitHub fork commit URL")
			}
		case "commit_ref":
			if strings.ContainsAny(text, " \t\n()") {
				block("基线版本不能混填 Tag 和 commit")
			}
		case "repo_url":
			u, err := url.Parse(text)
			if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(strings.Split(strings.Trim(u.Path, "/"), "/")) != 2 {
				block("必须是原始 GitHub 仓库地址")
			}
		case "trae_session_id_2", "seed_rounds":
			if present && value != nil && fmt.Sprint(value) != "" {
				block("此字段必须留空")
			}
		case "question_type":
			rounds, err := planInteger(plan.Payload["effective_rounds"])
			if err != nil || rounds <= 0 {
				block("缺少日志核验有效轮数")
			} else if rounds == 100 {
				block("恰为 100 轮，现有枚举不覆盖")
			} else if rounds > 100 && text != "有效轮数 > 100" || rounds < 100 && text == "有效轮数 > 100" {
				block("Type 与有效轮数不一致")
			} else if text == "有效轮数 < 100 且 效果好" && allResults && failures > 0 {
				block("效果好与 Rubric 失败结果不一致")
			} else if text == "有效轮数 < 100 且 效果差" {
				if allResults && failures == 0 {
					block("效果差却没有 Rubric 失败项")
				}
				if item.Status != "blocked" {
					item.Status = "review_required"
				}
			}
		case "requirement_completed":
			if allResults && (text == "完成" || text == "已完成") && failures > 0 {
				block("有 Rubric 未通过项，不能填写完成")
			}
			if allResults && failures == 0 && text != "完成" && text != "已完成" {
				block("全部 Rubric 通过时完成状态不一致")
			}
		case "authenticity_note", "involved_modules", "task_type", "seed_model", "effective_rounds", "x_is_more_than_two_hours", "trae_session_id":
			if item.Status != "blocked" {
				item.Status = "review_required"
			}
		}
		if len(item.Problems) > 0 {
			item.Status = "blocked"
		}
		report.Fields = append(report.Fields, item)
	}
	return report
}
func validateOneField(field FormField, value any) error {
	text, ok := value.(string)
	switch field.Type {
	case "text", "textarea", "date", "select", "url":
		if !ok {
			return fmt.Errorf("字段应为文本")
		}
		if field.MaxLength > 0 && len([]rune(text)) > field.MaxLength {
			return fmt.Errorf("超过字段长度限制")
		}
		if field.Type == "select" && !contains(field.Options, text) {
			return fmt.Errorf("不在当前平台枚举内")
		}
		if field.Type == "date" {
			if _, err := time.Parse("2006-01-02", text); err != nil {
				return fmt.Errorf("日期必须为 YYYY-MM-DD")
			}
		}
	case "number", "precheck_ref":
		n, err := planInteger(value)
		if err != nil || n < 0 || field.Type == "precheck_ref" && n == 0 {
			return fmt.Errorf("必须为整数")
		}
	}
	return nil
}
func FieldReviewFailures(report FieldReviewReport) []string {
	var problems []string
	for _, f := range report.Fields {
		for _, p := range f.Problems {
			problems = append(problems, f.Key+": "+p)
		}
	}
	return problems
}
