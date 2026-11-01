package precheckclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SubmissionPlan keeps local attachment paths outside the platform payload.
type SubmissionPlan struct {
	SchemaVersion string `json:"schema_version"`
	// SchemaSHA256 binds the draft to the exact local schema snapshot. The
	// version string alone is not sufficient when a platform reuses a version.
	SchemaSHA256         string            `json:"schema_sha256,omitempty"`
	ExistingSubmissionID int               `json:"existing_submission_id,omitempty"`
	ExecutionPrompt      string            `json:"execution_prompt"`
	Payload              map[string]any    `json:"payload"`
	Attachments          map[string]string `json:"attachments"`
}

// SubmissionSummary is a read-only view used to prevent duplicate creation.
type SubmissionSummary struct {
	Submission   SubmissionResult `json:"submission"`
	LockedFields []string         `json:"locked_fields"`
	Values       map[string]any   `json:"values"`
}

// FormSchemaSHA256 fingerprints the complete decoded schema in a stable JSON
// representation. It is used across local snapshots and live reads so a
// reused version label cannot silently change the audited form.
func FormSchemaSHA256(schema FormSchema) string {
	data, err := json.Marshal(schema)
	if err != nil {
		return ""
	}
	return TextSHA256(string(data))
}

// FormSchemaMatches reports whether schema is the bound form, ignoring the
// platform's open/closed gate flags. Quota and freeze windows flip those flags
// without changing any field rule, so a snapshot taken while submissions were
// limited must still match once the next cycle opens. The write gate itself is
// enforced separately by SubmitWorkflow.
func FormSchemaMatches(bound string, schema FormSchema) bool {
	if bound == "" {
		return false
	}
	for _, frozen := range []bool{false, true} {
		for _, limited := range []bool{false, true} {
			candidate := schema
			candidate.Frozen, candidate.Limited = frozen, limited
			if FormSchemaSHA256(candidate) == bound {
				return true
			}
		}
	}
	return false
}

func ValidateExistingSubmission(plan SubmissionPlan, existing SubmissionSummary) error {
	if existing.Submission.ID != plan.ExistingSubmissionID {
		return fmt.Errorf("existing submission identity differs; no uploads attempted")
	}
	for _, key := range existing.LockedFields {
		if value, ok := plan.Payload[key]; ok && fmt.Sprint(value) != fmt.Sprint(existing.Values[key]) {
			return fmt.Errorf("existing submission %d locks field %q; no uploads attempted", existing.Submission.ID, key)
		}
	}
	return fmt.Errorf("existing submission %d already exists; create is blocked to prevent duplication", existing.Submission.ID)
}

func ValidateSubmissionPlan(plan SubmissionPlan, schema FormSchema) error {
	if plan.ExistingSubmissionID < 0 {
		return fmt.Errorf("existing submission ID must not be negative")
	}
	if strings.TrimSpace(plan.ExecutionPrompt) == "" {
		return fmt.Errorf("actual execution prompt is required for precheck comparison")
	}
	data, err := json.Marshal(plan.Payload)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}
	if err := validateSubmissionPayload(payload); err != nil {
		return err
	}
	if schema.Frozen || schema.Limited {
		return fmt.Errorf("platform currently blocks submission")
	}
	if schema.Version == "" || schema.Version != plan.SchemaVersion {
		return fmt.Errorf("form schema version changed or is missing")
	}
	fields := map[string]FormField{}
	var failures []error
	for _, field := range schema.Fields {
		if field.Key == "" || fields[field.Key].Key != "" {
			return fmt.Errorf("form schema has an empty or duplicated field")
		}
		fields[field.Key] = field
	}
	for _, field := range schema.Fields {
		if field.Auto {
			// The platform derives the prompt from the verified precheck record.
			if field.Required && field.Key != "requirement_prompt" {
				failures = append(failures, fmt.Errorf("cannot audit required platform-owned field %q", field.Key))
			}
			continue
		}
		required := field.Required || field.Type == "precheck_ref" || (field.RequiredWhen.Field != "" && contains(field.RequiredWhen.Values, fmt.Sprint(payload[field.RequiredWhen.Field])))
		if !required {
			continue
		}
		if field.Type == "attachment" {
			if strings.TrimSpace(plan.Attachments[field.Key]) == "" {
				failures = append(failures, fmt.Errorf("missing required attachment %q", field.Key))
			}
		} else if field.Type == "file" {
			failures = append(failures, fmt.Errorf("required file field %q is unsupported; submission blocked", field.Key))
		} else if value, ok := payload[field.Key]; !ok || value == nil || isPlaceholder(fmt.Sprint(value)) {
			failures = append(failures, fmt.Errorf("missing or placeholder required field %q", field.Key))
		}
	}
	if err := errors.Join(failures...); err != nil {
		return err
	}
	for key, value := range payload {
		field, ok := fields[key]
		if !ok || field.Auto || field.Type == "file" {
			return fmt.Errorf("payload contains unknown or platform-owned field %q", key)
		}
		if field.Type == "attachment" {
			return fmt.Errorf("attachment %q must be provided as a local file in the plan", key)
		}
		text, isText := value.(string)
		switch field.Type {
		case "text", "textarea", "url", "date", "select":
			if !isText {
				return fmt.Errorf("field %q must be text", key)
			}
		case "number", "precheck_ref":
		default:
			return fmt.Errorf("field %q has an unsupported schema type", key)
		}
		if field.Type == "date" && text != "" {
			if _, err := time.Parse("2006-01-02", text); err != nil {
				return fmt.Errorf("field %q must be a calendar date", key)
			}
		}
		if key == "trae_session_id" || (key == "trae_session_id_2" && text != "") {
			if err := ValidateTraeSessionToken(text); err != nil {
				return fmt.Errorf("field %q: %w", key, err)
			}
		}
		if field.Type == "url" {
			u, err := url.ParseRequestURI(text)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				return fmt.Errorf("field %q must be an HTTP(S) URL", key)
			}
		}
		if field.MaxLength > 0 && isText && len([]rune(text)) > field.MaxLength {
			return fmt.Errorf("field %q exceeds maximum length", key)
		}
		if field.Type == "select" && !contains(field.Options, text) {
			return fmt.Errorf("field %q has an invalid selection", key)
		}
		if field.Type == "number" || field.Type == "precheck_ref" {
			n, ok := value.(float64)
			if !ok || math.IsNaN(n) || math.IsInf(n, 0) {
				return fmt.Errorf("field %q must be numeric", key)
			}
			if n > float64(math.MaxInt) || (field.Type == "precheck_ref" && (n <= 0 || math.Trunc(n) != n)) || ((key == "effective_rounds" || key == "seed_rounds") && (n < 0 || math.Trunc(n) != n)) {
				return fmt.Errorf("field %q has an invalid integer value", key)
			}
		}
	}
	for key, path := range plan.Attachments {
		if _, forbidden := forbiddenSubmissionKeys[strings.ToLower(strings.TrimSpace(key))]; forbidden {
			return fmt.Errorf("attachment contains forbidden internal field %q", key)
		}
		field, ok := fields[key]
		if !ok || field.Type != "attachment" || field.Auto {
			return fmt.Errorf("unknown attachment field %q", key)
		}
		if !schema.Attachment.Configured {
			return fmt.Errorf("platform attachments are not configured")
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("attachment %q is unavailable", key)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("attachment %q is not a regular file", key)
		}
		if info.Size() == 0 {
			return fmt.Errorf("attachment %q is empty", key)
		}
		file, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("attachment %q is unreadable", key)
		}
		_ = file.Close()
		if schema.Attachment.MaxMB > 0 && info.Size() > int64(schema.Attachment.MaxMB)*1024*1024 {
			return fmt.Errorf("attachment %q exceeds maximum size", key)
		}
		if schema.Attachment.Accept != "" && !contains(strings.Split(schema.Attachment.Accept, ","), strings.ToLower(filepath.Ext(path))) {
			return fmt.Errorf("attachment %q has an unsupported extension", key)
		}
	}
	if review := ReviewSubmissionFields(plan, schema); !review.Passed {
		return fmt.Errorf("填写字段不合规: %s", strings.Join(FieldReviewFailures(review), "; "))
	}
	return nil
}

func isPlaceholder(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "未提供", "待填写", "待补充", "待人工确认", "未获取", "unknown", "n/a", "null", "none", "todo", "tbd", "...", "…":
		return true
	}
	return false
}

// This is the complete format observed from the first Agent avatar in Trae CN.
// no_sid is a suffix flag, not evidence that the embedded chat ID is missing.
// Trae also renames the CN product label between releases ("TraeCode CN" in
// 3.3.x, "TRAE CN" from 3.4.x), so only that label is matched
// case-insensitively; the identity segments, version, environment flags and
// timestamp stay exact.
var traeTokenPattern = regexp.MustCompile(`^[0-9a-f]{16}:[0-9a-f]{32}_[0-9a-f]{24}\.[0-9a-f]{24}\.[0-9a-f]{24}:(?i:Trae(?:Code|Work)? CN)\.[0-9]+\.[0-9]+\.[0-9]+\.[^\r\n:]+\.T\([0-9]{4}/[0-9]{1,2}/[0-9]{1,2} [0-9]{2}:[0-9]{2}:[0-9]{2}\)$`)

// sessionTokenPrefix matches the label-independent opening of a Trae session
// token, so leaked telemetry is rejected even if the product label changes.
var sessionTokenPrefix = regexp.MustCompile(`[0-9a-f]{16}:[0-9a-f]{32}_`)

func ValidateTraeSessionToken(token string) error {
	if !traeTokenPattern.MatchString(token) || strings.ContainsAny(token, "…") || strings.Contains(token, "...") {
		return fmt.Errorf("complete original Trae CN session token is required; short IDs, placeholders and unsupported formats are rejected")
	}
	return nil
}

// SubmitPlan is the guarded entry point: audit every field before any upload.
func (c *Client) SubmitPlan(ctx context.Context, plan SubmissionPlan, schema FormSchema) (SubmissionResult, error) {
	if err := ValidateSubmissionPlan(plan, schema); err != nil {
		return SubmissionResult{}, err
	}
	if plan.ExistingSubmissionID > 0 {
		existing, err := c.SubmissionDetail(ctx, plan.ExistingSubmissionID)
		if err != nil {
			return SubmissionResult{}, fmt.Errorf("could not verify existing submission; no uploads attempted")
		}
		return SubmissionResult{}, ValidateExistingSubmission(plan, existing)
	}
	// ValidateSubmissionPlan normalizes numbers but does not mutate the caller.
	data, _ := json.Marshal(plan.Payload)
	var payload map[string]any
	_ = json.Unmarshal(data, &payload)
	id, ok := payload["precheck_item_id"].(float64)
	if !ok || id <= 0 || math.Trunc(id) != id || id > float64(math.MaxInt) {
		return SubmissionResult{}, fmt.Errorf("invalid precheck item ID; no uploads attempted")
	}
	item, err := c.ItemDetail(ctx, int(id))
	if err != nil {
		return SubmissionResult{}, fmt.Errorf("could not verify precheck record; no uploads attempted")
	}
	if item.Status != "PRECHECK_PASSED" || item.RepoURL != payload["repo_url"] {
		return SubmissionResult{}, fmt.Errorf("precheck did not pass or repository differs; no uploads attempted")
	}
	if item.RequirementPrompt != plan.ExecutionPrompt {
		return SubmissionResult{}, fmt.Errorf("precheck prompt differs from the actual Trae execution prompt; no uploads attempted")
	}
	keys := make([]string, 0, len(plan.Attachments))
	for key := range plan.Attachments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		upload, err := c.UploadAttachment(ctx, plan.Attachments[key])
		if err != nil || upload.URL == "" {
			return SubmissionResult{}, fmt.Errorf("upload %q failed or outcome is uncertain; no automatic retry, submission not attempted", key)
		}
		payload[key] = upload.URL
	}
	result, err := c.CreateSubmission(ctx, payload, "", plan.SchemaVersion)
	if err != nil {
		return SubmissionResult{}, fmt.Errorf("submission failed or outcome is uncertain; inspect platform before retrying")
	}
	return result, nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == expected {
			return true
		}
	}
	return false
}
