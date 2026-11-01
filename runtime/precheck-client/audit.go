package precheckclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type AuditCheck struct {
	Passed bool   `json:"passed"`
	Code   string `json:"code,omitempty"`
}

type SubmissionAudit struct {
	FieldReview          FieldReviewReport     `json:"field_review"`
	SchemaVersion        int                   `json:"schema_version"`
	Passed               bool                  `json:"passed"`
	CheckedAt            string                `json:"checked_at"`
	NetworkWrites        int                   `json:"network_writes"`
	PlanSHA256           string                `json:"plan_sha256"`
	Checks               map[string]AuditCheck `json:"checks"`
	Attachments          map[string]string     `json:"attachment_sha256"`
	PrecheckItemID       int                   `json:"precheck_item_id"`
	ExistingSubmissionID int                   `json:"existing_submission_id,omitempty"`
	RemarksSent          bool                  `json:"remarks_sent"`
}

type SessionEvidence struct {
	TokenSHA256       string `json:"token_sha256"`
	WorkspaceVerified bool   `json:"workspace_verified"`
	WorkspaceSHA256   string `json:"workspace_sha256"`
	// EffectiveRoundCount is the count the receipt is authoritative for: live
	// plan events for a log-backed capture, deduplicated tool calls for a
	// historical one. Source and SessionSource record which.
	EffectiveRoundCount int    `json:"effective_round_count"`
	MatchingLogCount    int    `json:"matching_log_count"`
	Applied             bool   `json:"applied"`
	Source              string `json:"source,omitempty"`
	SessionSource       string `json:"session_source,omitempty"`
	RoundsSource        string `json:"rounds_source,omitempty"`
	ExclusionApplied    *bool  `json:"exclusion_applied,omitempty"`
}

type NativeTerminalEvidence struct {
	SchemaVersion      int                `json:"schema_version"`
	DisplayPath        string             `json:"display_path"`
	DisplaySHA256      string             `json:"display_sha256"`
	DisplayMode        string             `json:"display_mode"`
	Source             string             `json:"source"`
	Completed          bool               `json:"completed"`
	ExecutionCompleted bool               `json:"execution_completed"`
	ExitCode           *int               `json:"exit_code"`
	ErrorCode          string             `json:"error_code,omitempty"`
	CapturedAt         string             `json:"captured_at"`
	PrivacyCrop        *NativePrivacyCrop `json:"privacy_crop"`
	ScreenshotSHA256   string             `json:"screenshot_sha256"`
	LogSHA256          string             `json:"log_sha256"`
	CommandSHA256      string             `json:"command_sha256"`
	CommandPath        string             `json:"command_path"`
	LogPath            string             `json:"log_path"`
}

type NativePrivacyCrop struct {
	Reason                string `json:"reason"`
	PixelBox              []int  `json:"pixel_box"`
	ContentPixelsModified bool   `json:"content_pixels_modified"`
}

func validNativePrivacyCrop(crop *NativePrivacyCrop) bool {
	return crop != nil && crop.Reason != "" && !crop.ContentPixelsModified && len(crop.PixelBox) == 4 && crop.PixelBox[0] >= 0 && crop.PixelBox[1] > 0 && crop.PixelBox[2] > crop.PixelBox[0] && crop.PixelBox[3] > crop.PixelBox[1]
}

func validEvidenceRelativePath(path string) bool {
	clean := filepath.Clean(path)
	return path != "" && !filepath.IsAbs(path) && clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

type TraceEvidence struct {
	Source        string `json:"source"`
	SHA256        string `json:"sha256"`
	PromptSHA256  string `json:"prompt_sha256"`
	SessionSHA256 string `json:"session_sha256"`
}

type AuditEvidence struct {
	Session           SessionEvidence
	Terminal          NativeTerminalEvidence
	Trace             TraceEvidence
	Workspace         string
	TerminalDirectory string
}

func PlanSHA256(plan SubmissionPlan) string {
	data, _ := json.Marshal(plan)
	return TextSHA256(string(data))
}

func ReadSubmissionPlan(path string) (SubmissionPlan, error) {
	var plan SubmissionPlan
	if err := ReadLocalJSON(path, &plan); err != nil {
		return plan, err
	}
	for key, value := range plan.Attachments {
		if !filepath.IsAbs(value) {
			plan.Attachments[key] = filepath.Join(filepath.Dir(path), value)
		}
	}
	return plan, nil
}

func ReadLocalJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("local JSON unavailable")
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("local JSON invalid")
	}
	return nil
}

func AuditSubmission(plan SubmissionPlan, schema FormSchema, evidence AuditEvidence) SubmissionAudit {
	r := SubmissionAudit{SchemaVersion: 1, CheckedAt: time.Now().UTC().Format(time.RFC3339), PlanSHA256: PlanSHA256(plan), Checks: map[string]AuditCheck{}, Attachments: map[string]string{}, ExistingSubmissionID: plan.ExistingSubmissionID}
	check := func(key string, passed bool, code string) {
		r.Checks[key] = AuditCheck{Passed: passed}
		if !passed {
			r.Checks[key] = AuditCheck{Code: code}
		}
	}
	// A limited platform may still be audited locally. Keep the final write
	// gate in SubmitWorkflow, while allowing evidence collection and dry-runs.
	validationSchema := schema
	validationSchema.Limited = false
	check("required_fields_and_schema", ValidateSubmissionPlan(plan, validationSchema) == nil, "invalid_fields_or_schema")
	r.FieldReview = ReviewSubmissionFields(plan, schema)
	for _, field := range r.FieldReview.Fields {
		check("field_"+field.Key, field.Status != "blocked", "field_not_compliant")
	}
	check("create_unique", plan.ExistingSubmissionID == 0, "existing_submission")
	token, _ := plan.Payload["trae_session_id"].(string)
	normalized, _ := json.Marshal(plan.Payload)
	var payload map[string]any
	_ = json.Unmarshal(normalized, &payload)
	rounds, roundsOK := payload["effective_rounds"].(float64)
	if n, ok := payload["precheck_item_id"].(float64); ok {
		r.PrecheckItemID = int(n)
	}
	workspace, _ := filepath.Abs(evidence.Workspace)
	s := evidence.Session
	// A historical receipt proves the run from Trae's per-session snapshot
	// repository, so it has no matched renderer log by construction; a
	// log-backed receipt still has to carry one.
	historicalReceipt := s.SessionSource == "trae_session_snapshot" || s.Source == "historical_session_snapshot"
	check("session_log_verified", token != "" && s.Applied && s.WorkspaceVerified && s.TokenSHA256 == TextSHA256(token) && roundsOK && rounds > 0 && rounds == float64(s.EffectiveRoundCount) && (s.MatchingLogCount > 0 || historicalReceipt) && s.WorkspaceSHA256 == TextSHA256(workspace), "session_evidence_mismatch")
	for key, path := range plan.Attachments {
		data, err := os.ReadFile(path)
		check("attachment_"+key, err == nil && len(data) > 0, "attachment_unavailable")
		if err != nil {
			continue
		}
		hash := TextSHA256(string(data))
		r.Attachments[key] = hash
		switch key {
		case "x_artifact_shot":
			_, pngErr := png.DecodeConfig(bytes.NewReader(data))
			t := evidence.Terminal
			log, logErr := HashWorkflowArtifact(filepath.Join(evidence.TerminalDirectory, t.LogPath))
			command, commandErr := HashWorkflowArtifact(filepath.Join(evidence.TerminalDirectory, t.CommandPath))
			_, capturedErr := time.Parse(time.RFC3339Nano, t.CapturedAt)
			check("native_terminal", pngErr == nil && t.Source == "native_terminal" && t.Completed && t.ExecutionCompleted && t.ExitCode != nil && t.ErrorCode == "" && capturedErr == nil && validNativePrivacyCrop(t.PrivacyCrop) && t.ScreenshotSHA256 == hash && validEvidenceRelativePath(t.LogPath) && validEvidenceRelativePath(t.CommandPath) && logErr == nil && log.SHA256 == t.LogSHA256 && commandErr == nil && command.SHA256 == t.CommandSHA256, "terminal_evidence_mismatch")
			check("native_display_binding", validNativeDisplay(evidence.TerminalDirectory, t), "terminal_display_mismatch")
		case "x_trae_trace":
			t := evidence.Trace
			check("trae_app_export", t.Source == "trae_cn_app_export" && t.SHA256 == hash && t.PromptSHA256 == TextSHA256(plan.ExecutionPrompt) && t.SessionSHA256 == TextSHA256(token) && !strings.Contains(string(data), token) && strings.Contains(string(data), plan.ExecutionPrompt), "trace_evidence_mismatch")
		}
	}
	for _, key := range []string{"x_artifact_shot", "x_trae_trace"} {
		if _, ok := plan.Attachments[key]; !ok {
			check("attachment_"+key, false, "required_evidence_missing")
		}
	}
	check("remarks_excluded", validateSubmissionPayload(plan.Payload) == nil, "internal_fields_in_payload")
	r.Passed = true
	for _, c := range r.Checks {
		if !c.Passed {
			r.Passed = false
		}
	}
	return r
}

func validNativeDisplay(directory string, t NativeTerminalEvidence) bool {
	// Legacy manifests remain readable; new captures must bind the visible text.
	if t.SchemaVersion == 0 || t.SchemaVersion == 1 {
		return true
	}
	if t.SchemaVersion != 2 || !validEvidenceRelativePath(t.DisplayPath) || (t.DisplayMode != "rubric_results" && t.DisplayMode != "log_tail") {
		return false
	}
	display, err := HashWorkflowArtifact(filepath.Join(directory, t.DisplayPath))
	return err == nil && display.SHA256 == t.DisplaySHA256
}

// AuditPrecheck does reads only; a cached/local check cannot grant this result.
func (c *Client) AuditPrecheck(ctx context.Context, plan SubmissionPlan, report *SubmissionAudit) error {
	item, err := c.ItemDetail(ctx, report.PrecheckItemID)
	if err != nil {
		report.Checks["precheck_live"] = AuditCheck{Code: "precheck_read_failed"}
		report.Passed = false
		return fmt.Errorf("precheck read failed")
	}
	ok := item.Status == "PRECHECK_PASSED" && item.RepoURL == plan.Payload["repo_url"] && item.RequirementPrompt == plan.ExecutionPrompt
	report.Checks["precheck_live"] = AuditCheck{Passed: ok}
	if !ok {
		report.Checks["precheck_live"] = AuditCheck{Code: "precheck_mismatch"}
		report.Passed = false
	}
	return nil
}

func AuditFailures(report SubmissionAudit) []string {
	var failures []string
	for key, c := range report.Checks {
		if !c.Passed {
			failures = append(failures, key+":"+c.Code)
		}
	}
	sort.Strings(failures)
	return failures
}
