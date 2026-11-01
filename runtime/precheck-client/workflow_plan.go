package precheckclient

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// PrepareWorkflowPlan copies verified workflow bindings into a draft. Missing
// session/attachments are allowed here; submission still requires the audit.
func PrepareWorkflowPlan(state WorkflowState, template SubmissionPlan, schema FormSchema, prompt string) (SubmissionPlan, error) {
	switch state.Status {
	case WorkflowPrecheckPassed, WorkflowTraeRunning, WorkflowSessionCaptured, WorkflowSessionVerified:
	default:
		return SubmissionPlan{}, fmt.Errorf("task stage does not allow preparing a submission draft")
	}
	if state.Submission.ID != 0 || template.ExistingSubmissionID != 0 {
		return SubmissionPlan{}, fmt.Errorf("existing submission cannot be prepared as a new draft")
	}
	if state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain") {
		return SubmissionPlan{}, fmt.Errorf("unresolved platform write blocks draft preparation")
	}
	// Preparing a local draft does not write to the platform. A temporary
	// submission limit must block final submission, but it must not prevent
	// collecting the execution evidence needed to complete the draft.
	if schema.Frozen || strings.TrimSpace(schema.Version) == "" {
		return SubmissionPlan{}, fmt.Errorf("current form schema is unavailable or frozen")
	}
	// A prompt that the platform already froze for this precheck is authoritative:
	// the exact SHA-256 below is re-checked against the platform again at submit
	// time. The local 240-rune drafting rule (ValidateDraftPrompt) only governs
	// prompts composed here, and legacy prechecks may carry a longer prompt, so
	// the length must not be re-asserted on an already bound prompt.
	if strings.TrimSpace(prompt) == "" || !workflowHash.MatchString(state.Precheck.PromptSHA256) || TextSHA256(prompt) != state.Precheck.PromptSHA256 {
		return SubmissionPlan{}, fmt.Errorf("prompt must be the exact verified precheck text")
	}
	if template.ExecutionPrompt != "" && template.ExecutionPrompt != prompt {
		return SubmissionPlan{}, fmt.Errorf("draft execution prompt differs from the verified precheck")
	}
	if state.Precheck.ItemID <= 0 || isPlaceholder(state.Project.RepoURL) || isPlaceholder(state.Project.BaselineCommit) || isPlaceholder(state.Project.Language) {
		return SubmissionPlan{}, fmt.Errorf("workflow lacks a verified precheck, repository, baseline or language")
	}
	if state.Trae.EffectiveRounds != nil && *state.Trae.EffectiveRounds < 0 {
		return SubmissionPlan{}, fmt.Errorf("workflow has an invalid effective round count")
	}
	encoded, err := json.Marshal(template)
	if err != nil {
		return SubmissionPlan{}, fmt.Errorf("submission draft is not JSON-compatible")
	}
	var result SubmissionPlan
	if err := json.Unmarshal(encoded, &result); err != nil {
		return SubmissionPlan{}, fmt.Errorf("submission draft is invalid")
	}
	if result.Payload == nil {
		result.Payload = map[string]any{}
	}
	if err := validateSubmissionPayload(result.Payload); err != nil {
		return SubmissionPlan{}, fmt.Errorf("submission draft contains forbidden internal fields")
	}
	fields := make(map[string]FormField, len(schema.Fields))
	for _, field := range schema.Fields {
		if field.Key == "" || fields[field.Key].Key != "" {
			return SubmissionPlan{}, fmt.Errorf("form schema contains an empty or duplicate field")
		}
		fields[field.Key] = field
	}
	for key := range result.Payload {
		field, exists := fields[key]
		if !exists || field.Auto || field.Type == "file" || field.Type == "attachment" {
			return SubmissionPlan{}, fmt.Errorf("submission draft contains an unsupported platform field")
		}
	}
	for key := range result.Attachments {
		if _, forbidden := forbiddenSubmissionKeys[strings.ToLower(strings.TrimSpace(key))]; forbidden {
			return SubmissionPlan{}, fmt.Errorf("submission draft contains a forbidden attachment field")
		}
		field, exists := fields[key]
		if !exists || field.Auto || field.Type != "attachment" {
			return SubmissionPlan{}, fmt.Errorf("submission draft contains an unsupported attachment field")
		}
	}
	bindings := map[string]any{
		"precheck_item_id": state.Precheck.ItemID,
		"repo_url":         state.Project.RepoURL,
		"commit_ref":       state.Project.BaselineCommit,
		"primary_language": state.Project.Language,
	}
	if strings.TrimSpace(state.Trae.Model) != "" {
		bindings["seed_model"] = state.Trae.Model
	}
	if state.Trae.EffectiveRounds != nil {
		bindings["effective_rounds"] = *state.Trae.EffectiveRounds
	}
	for key, value := range bindings {
		field, exists := fields[key]
		if !exists || field.Auto || field.Type == "file" || field.Type == "attachment" {
			return SubmissionPlan{}, fmt.Errorf("current form schema cannot accept workflow binding %q", key)
		}
		if current, exists := result.Payload[key]; exists && !emptyDraftBinding(key, current) && !sameDraftBinding(current, value) {
			return SubmissionPlan{}, fmt.Errorf("draft field %q conflicts with verified task state", key)
		}
		if text, ok := value.(string); ok {
			if field.Type != "text" && field.Type != "textarea" && field.Type != "url" && field.Type != "select" {
				return SubmissionPlan{}, fmt.Errorf("current form schema has an incompatible workflow field %q", key)
			}
			if field.MaxLength > 0 && len([]rune(text)) > field.MaxLength || field.Type == "select" && !contains(field.Options, text) {
				return SubmissionPlan{}, fmt.Errorf("workflow binding does not fit current form field %q", key)
			}
		} else if field.Type != "number" && field.Type != "precheck_ref" {
			return SubmissionPlan{}, fmt.Errorf("current form schema has an incompatible workflow field %q", key)
		}
		result.Payload[key] = value
	}
	result.ExecutionPrompt = prompt
	result.SchemaVersion = schema.Version
	return result, nil
}

func emptyDraftBinding(key string, value any) bool {
	if value == nil {
		return true
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text) == ""
	}
	if key == "effective_rounds" {
		n, ok := value.(float64)
		return ok && n == 0
	}
	return false
}

func sameDraftBinding(a, b any) bool {
	if expected, ok := b.(int); ok {
		actual, ok := a.(float64)
		return ok && !math.IsNaN(actual) && !math.IsInf(actual, 0) && actual == float64(expected)
	}
	actual, actualOK := a.(string)
	expected, expectedOK := b.(string)
	return actualOK && expectedOK && actual == expected
}
