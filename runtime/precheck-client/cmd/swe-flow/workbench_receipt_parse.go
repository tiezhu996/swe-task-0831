package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	client "example.com/swe-precheck-client"
)

func classifyReceipt(payload map[string]any) string {
	has := func(key string) bool { _, ok := payload[key]; return ok }
	switch {
	case has("server_verification") || has("session_qc_checks") || (has("rejection_reason") && has("new_submission_id")):
		return "submission_qc"
	case has("platform_status") && has("submission_id"):
		return "platform"
	case has("batch_id") && has("requirement_prompt_sha256"):
		return "precheck"
	case has("session_sha256") && (has("prompt_exact_match") || has("message_count") || has("user_message_count")):
		return "trace_export"
	case has("session_token_sha256") || (has("effective_rounds") && has("result")):
		return "session_rounds"
	case has("log_path") && has("execution_started_at"):
		return "execution"
	case has("commit_url") && (has("branch") || has("fork_url")):
		return "publication"
	case has("checks") && has("submission"):
		return "gate"
	default:
		return "unknown"
	}
}

func receiptReason(payload map[string]any) string {
	if value := digString(payload, "rejection_reason"); value != "" {
		return value
	}
	if value := digString(payload, "server_verification", "rejection_reason"); value != "" {
		return value
	}
	for _, key := range []string{"reason", "note", "message"} {
		if value := digString(payload, key); value != "" {
			return value
		}
	}
	if value, ok := payload["result"].(string); ok && !looksLikeStatus(value) {
		return value
	}
	return ""
}

func looksLikeStatus(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "PASS", "PASSED", "OK", "FAIL", "FAILED", "SUCCESS", "SUCCEEDED", "PENDING", "RUNNING":
		return true
	default:
		return false
	}
}

func receiptChecks(payload map[string]any) []receiptCheckView {
	var raw []any
	if nested, ok := payload["server_verification"].(map[string]any); ok {
		raw, _ = nested["session_qc_checks"].([]any)
	}
	if raw == nil {
		raw, _ = payload["checks"].([]any)
	}
	checks := []receiptCheckView{}
	for _, item := range raw {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		check := receiptCheckView{
			Key:        digString(entry, "key"),
			Name:       digString(entry, "name"),
			Category:   digString(entry, "category"),
			Conclusion: digString(entry, "conclusion"),
			Summary:    digString(entry, "summary"),
			Detail:     digString(entry, "detail"),
			DurationMS: int(digNumber(entry, "duration_ms")),
		}
		if check.Conclusion == "" {
			check.Conclusion = digString(entry, "status")
		}
		checks = append(checks, check)
		if len(checks) >= 24 {
			break
		}
	}
	return checks
}

func platformConclusion(items []receiptView) (string, string) {
	for _, kind := range []string{"submission_qc", "platform"} {
		for _, item := range items {
			if item.Kind == kind && item.Reason != "" {
				return item.Reason, item.Status
			}
		}
	}
	for _, item := range items {
		if item.Reason != "" {
			return item.Reason, item.Status
		}
	}
	return "", ""
}

var localRubricPattern = regexp.MustCompile(`(?m)^\s*(\d+)\s*[、.)]?\s*(通过|未通过|不通过|失败)\s*(.*)$`)

func readLocalAssessment(state client.WorkflowState) localAssessment {
	if state.PlanPath == "" {
		return localAssessment{Available: false, Note: "任务没有提交计划"}
	}
	plan, err := client.ReadSubmissionPlan(state.PlanPath)
	if err != nil {
		return localAssessment{Available: false, Note: "提交计划不可读"}
	}
	text, _ := plan.Payload["artifact_result"].(string)
	if strings.TrimSpace(text) == "" {
		return localAssessment{Available: false, Note: "提交计划没有逐条自评"}
	}
	assessment := localAssessment{Available: true, Source: "submission-plan.payload.artifact_result"}
	for _, match := range localRubricPattern.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(match[1])
		ok := match[2] == "通过"
		line := localRubricLine{ID: id, OK: ok, Text: strings.TrimSpace(match[3])}
		assessment.Lines = append(assessment.Lines, line)
		if ok {
			assessment.Passed++
		} else {
			assessment.Failed++
		}
	}
	assessment.Total = len(assessment.Lines)
	if assessment.Total == 0 {
		assessment.Available = false
		assessment.Note = "逐条自评无法解析"
	}
	return assessment
}

func receiptConflicts(local localAssessment, platformReason, submissionStatus string) []evidenceConflict {
	conflicts := []evidenceConflict{}
	if !local.Available || local.Total == 0 {
		return conflicts
	}
	status := strings.ToUpper(strings.TrimSpace(submissionStatus))
	passed := status == "QC_PASSED" || status == "PASSED" || status == "ACCEPTED"
	failed := status == "PENDING_FIX" || status == "REJECTED"
	localText := fmt.Sprintf("本地自评 %d/%d 通过", local.Passed, local.Total)
	platformText := platformReason
	if platformText == "" {
		platformText = "平台状态 " + status
	}
	if local.Failed == 0 && failed {
		conflicts = append(conflicts, evidenceConflict{
			Kind:     "self_vs_platform",
			Local:    localText,
			Platform: platformText,
			Note:     "本地自评全部通过但平台退回，必须并列显示，不能选一个",
		})
	}
	if local.Failed > 0 && passed {
		conflicts = append(conflicts, evidenceConflict{
			Kind:     "self_vs_platform",
			Local:    fmt.Sprintf("本地自评有 %d 条未通过", local.Failed),
			Platform: platformText,
			Note:     "本地自评与平台通过结论不一致；本地计划可能已过期，不能直接判定材料有效",
		})
	}
	return conflicts
}
