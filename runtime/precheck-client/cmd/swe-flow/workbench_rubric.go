package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	client "example.com/swe-precheck-client"
	"gopkg.in/yaml.v3"
)

// ---------- rubric ----------

type rubricItemView struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Text       string `json:"text"`
	Conclusion string `json:"conclusion"`
	Evidence   string `json:"evidence,omitempty"`
	Source     string `json:"source,omitempty"`
}

type rubricSummary struct {
	Total       int      `json:"total"`
	Passed      int      `json:"passed"`
	Failed      int      `json:"failed"`
	Missing     int      `json:"missing"`
	Unbound     int      `json:"unbound"`
	Source      string   `json:"source,omitempty"`
	Script      string   `json:"script,omitempty"`
	ScriptTests []string `json:"script_tests,omitempty"`
	Error       string   `json:"error,omitempty"`
}

type rubricFile struct {
	Rubrics []struct {
		ID   int    `yaml:"id"`
		Type string `yaml:"type"`
		Text string `yaml:"text"`
	} `yaml:"rubrics"`
}

func rubricEvidence(taskPath string, state client.WorkflowState) ([]rubricItemView, rubricSummary) {
	summary := rubricSummary{}
	taskDir := filepath.Dir(taskPath)
	items := []rubricItemView{}
	rubricPath := filepath.Join(taskDir, "design", "rubric.yaml")
	data, err := os.ReadFile(rubricPath)
	if err != nil {
		summary.Error = "rubric.yaml 不存在"
		return items, summary
	}
	var parsed rubricFile
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		summary.Error = "rubric.yaml 无法解析"
		return items, summary
	}
	results, resultSource, resultErr := rubricResults(taskDir, state)
	if resultErr != "" {
		summary.Error = resultErr
	}
	summary.Source = resultSource
	scriptPath := filepath.Join(taskDir, "verification", "verify-rubrics.sh")
	if script, err := os.ReadFile(scriptPath); err == nil {
		summary.Script = scriptPath
		seen := map[string]bool{}
		for _, match := range regexp.MustCompile(`Test[A-Za-z0-9_]+`).FindAllString(string(script), -1) {
			if !seen[match] {
				seen[match] = true
				summary.ScriptTests = append(summary.ScriptTests, match)
			}
		}
		sort.Strings(summary.ScriptTests)
	}
	for _, item := range parsed.Rubrics {
		view := rubricItemView{ID: item.ID, Type: item.Type, Text: item.Text, Conclusion: "UNBOUND"}
		if result, ok := results[item.ID]; ok {
			view.Conclusion = result.Conclusion
			view.Evidence = result.Evidence
			view.Source = resultSource
		} else if resultSource != "" {
			view.Conclusion = "MISSING"
		}
		switch view.Conclusion {
		case "PASS":
			summary.Passed++
		case "FAIL":
			summary.Failed++
		case "MISSING":
			summary.Missing++
		default:
			summary.Unbound++
		}
		items = append(items, view)
	}
	summary.Total = len(items)
	return items, summary
}

type rubricResult struct {
	Conclusion string
	Evidence   string
}

func rubricResults(taskDir string, state client.WorkflowState) (map[int]rubricResult, string, string) {
	candidates := []string{}
	if state.Verification.RubricReportPath != "" && pathContains(taskDir, state.Verification.RubricReportPath) {
		candidates = append(candidates, state.Verification.RubricReportPath)
	}
	candidates = append(candidates,
		filepath.Join(taskDir, "verification", "rubric-results.json"),
		filepath.Join(taskDir, "verification", "rubric-report.json"),
	)
	for _, path := range candidates {
		if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() || info.Size() > receiptMaxBytes {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			continue
		}
		raw, _ := payload["results"].([]any)
		if raw == nil {
			continue
		}
		results := map[int]rubricResult{}
		for _, entry := range raw {
			item, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			id := int(digNumber(item, "id"))
			if id <= 0 {
				continue
			}
			conclusion := strings.ToUpper(digString(item, "status"))
			if conclusion == "" {
				if passed, ok := item["passed"].(bool); ok {
					if passed {
						conclusion = "PASS"
					} else {
						conclusion = "FAIL"
					}
				}
			}
			if conclusion == "" {
				conclusion = "UNKNOWN"
			}
			evidence := digString(item, "reason")
			if detail, ok := item["details"]; ok && evidence == "" {
				if encoded, err := json.Marshal(detail); err == nil {
					evidence = string(encoded)
				}
			}
			if list, ok := item["evidence"].([]any); ok && evidence == "" {
				parts := []string{}
				for _, value := range list {
					if text, ok := value.(string); ok {
						parts = append(parts, text)
					}
				}
				evidence = strings.Join(parts, "；")
			}
			if len(evidence) > 300 {
				evidence = evidence[:300] + "…"
			}
			results[id] = rubricResult{Conclusion: conclusion, Evidence: evidence}
		}
		return results, path, ""
	}
	return map[int]rubricResult{}, "", "找不到 rubric 结果文件，全部判据未绑定"
}
