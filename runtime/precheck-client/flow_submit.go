package precheckclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func BeginWorkflowWrite(ctx context.Context, store *WorkflowStore, state WorkflowState, kind, endpoint, payloadHash string) (WorkflowState, error) {
	return beginWorkflowWrite(ctx, store, state, kind, endpoint, payloadHash, nil, nil, false)
}

func BeginPrecheckWrite(ctx context.Context, store *WorkflowStore, state WorkflowState, kind, endpoint, payloadHash string, before *WorkflowPrecheck, priorIDs []int) (WorkflowState, error) {
	if kind != "precheck_submit" && kind != "precheck_repair" && kind != "precheck_rerun" {
		return state, fmt.Errorf("unsupported precheck write")
	}
	if kind != "precheck_submit" && before == nil {
		return state, fmt.Errorf("precheck write requires a verified previous identity")
	}
	return beginWorkflowWrite(ctx, store, state, kind, endpoint, payloadHash, before, priorIDs, true)
}

func beginWorkflowWrite(ctx context.Context, store *WorkflowStore, state WorkflowState, kind, endpoint, payloadHash string, before *WorkflowPrecheck, priorIDs []int, snapshot bool) (WorkflowState, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return state, err
	}
	id := hex.EncodeToString(random[:])
	return store.Update(ctx, state.Revision, WorkflowAudit{Action: kind + "_started", Method: writeMethod(kind), Endpoint: endpoint, ExternalWrite: true, OperationID: id, Outcome: "started", PayloadSHA256: payloadHash}, func(s *WorkflowState) error {
		if s.PendingOperation != nil && (s.PendingOperation.Outcome == "started" || s.PendingOperation.Outcome == "uncertain") {
			return fmt.Errorf("unresolved write; read platform status before any retry")
		}
		s.PendingOperation = &WorkflowOperation{ID: id, Kind: kind, PayloadSHA256: payloadHash, StartedAt: time.Now().UTC().Format(time.RFC3339), Outcome: "started", PrecheckBefore: before, PriorPrecheckItemIDs: priorIDs, PrecheckSnapshotTaken: snapshot}
		if kind == "submit" {
			createStarted := false
			s.PendingOperation.CreateRequestStarted = &createStarted
			s.Status = WorkflowSubmitting
		}
		return nil
	})
}

// ResetUncreatedSubmission only uses explicit durable evidence that create was
// never started. Uploaded attachments may remain orphaned on the platform.
func ResetUncreatedSubmission(ctx context.Context, store *WorkflowStore) (WorkflowState, error) {
	state, err := store.Load(ctx)
	if err != nil {
		return state, err
	}
	op := state.PendingOperation
	if state.Status != WorkflowSubmitting || state.Submission.ID != 0 || op == nil || op.Kind != "submit" || (op.Outcome != "started" && op.Outcome != "uncertain") {
		return state, fmt.Errorf("no uncreated submission operation is eligible for local reset")
	}
	if op.CreateRequestStarted == nil || *op.CreateRequestStarted {
		return state, fmt.Errorf("cannot prove create was never started; reconcile with the platform before retrying")
	}
	return store.Update(ctx, state.Revision, WorkflowAudit{Action: "reconcile", OperationID: op.ID, Outcome: "reconciled", PayloadSHA256: op.PayloadSHA256}, func(next *WorkflowState) error {
		next.Status = WorkflowDryRunPassed
		next.PendingOperation = nil
		return nil
	})
}

func FinishWorkflowWrite(ctx context.Context, store *WorkflowStore, state WorkflowState, endpoint string, resultID int, success bool, mutate func(*WorkflowState) error) (WorkflowState, error) {
	if state.PendingOperation == nil {
		return state, fmt.Errorf("write operation is missing")
	}
	op := state.PendingOperation
	outcome := "uncertain"
	if success {
		outcome = "success"
	}
	return store.Update(ctx, state.Revision, WorkflowAudit{Action: op.Kind + "_finished", Method: writeMethod(op.Kind), Endpoint: endpoint, ExternalWrite: true, OperationID: op.ID, Outcome: outcome, ResultID: resultID, PayloadSHA256: op.PayloadSHA256}, func(s *WorkflowState) error {
		s.PendingOperation.Outcome = outcome
		if success && mutate != nil {
			return mutate(s)
		}
		return nil
	})
}

func writeMethod(kind string) string {
	if kind == "precheck_repair" {
		return "PUT"
	}
	return "POST"
}

// MinimumSubmittedRounds is the platform floor for a submittable run. A
// completed task whose effective round count stays below it is kept on this
// machine on purpose and never uploaded.
const MinimumSubmittedRounds = 100

// SubmissionRounds reads the effective round count recorded in a plan. known is
// false when the field is missing or is not a non-negative integer.
func SubmissionRounds(plan SubmissionPlan) (rounds int, known bool) {
	// Plans read from disk carry JSON numbers as float64; a plan built in Go may
	// carry a plain int. Both are accepted, and only non-negative integers pass.
	switch value := plan.Payload["effective_rounds"].(type) {
	case float64:
		if value < 0 || value != float64(int(value)) {
			return 0, false
		}
		return int(value), true
	case int:
		if value < 0 {
			return 0, false
		}
		return value, true
	}
	return 0, false
}

// Question type and completion values the platform uses to describe a run.
const (
	questionTypeGoodEffect = "有效轮数 < 100 且 效果好"
	questionTypeBadEffect  = "有效轮数 < 100 且 效果差"
	questionTypeOverFloor  = "有效轮数 > 100"
	completionDone         = "完成"
)

// LocalOnlyEligible reports whether a status has reached the point where the
// round floor and the evaluation decide what happens to the task.
func LocalOnlyEligible(status WorkflowStatus) bool {
	switch status {
	case WorkflowSessionVerified, WorkflowArtifactsVerified, WorkflowDryRunPassed:
		return true
	}
	return false
}

// LocalOnlyMarkable reports whether an operator may abandon this state by hand.
// Every state before the first platform write qualifies: a task that has not
// been submitted can always be dropped. The automatic decision is narrower and
// only covers states where the run's result is already known.
func LocalOnlyMarkable(status WorkflowStatus) bool {
	switch status {
	case WorkflowInitialized, WorkflowProjectSelected, WorkflowPrecheckDraft,
		WorkflowPrecheckSubmitted, WorkflowPrecheckRejected, WorkflowPrecheckPassed,
		WorkflowTraeRunning, WorkflowSessionCaptured, WorkflowSessionVerified,
		WorkflowArtifactsVerified, WorkflowDryRunPassed:
		return true
	}
	return false
}

// LocalOnlyDecision reports whether a finished run must stay on this machine
// because the platform will not collect it, together with the reason.
//
// The collection round prioritises runs above the floor and keeps both
// completed and unfinished ones. Below the floor only a real failure is
// collected: an ideal implementation ("效果好") or a satisfied requirement
// ("完成") is a control sample that never becomes a submission. A run below the
// floor with a bad effect and an unfinished requirement stays submittable.
func LocalOnlyDecision(state WorkflowState, plan SubmissionPlan) (bool, string) {
	if state.Submission.ID > 0 || state.Trae.EffectiveRounds == nil {
		return false, ""
	}
	rounds := *state.Trae.EffectiveRounds
	if rounds >= MinimumSubmittedRounds {
		return false, ""
	}
	effect, _ := plan.Payload["question_type"].(string)
	completed, _ := plan.Payload["requirement_completed"].(string)
	if effect == questionTypeGoodEffect || completed == completionDone {
		return true, fmt.Sprintf("有效轮数 %d 不足 %d，且评测为效果好或已完成；平台不收录，仅留存本地", rounds, MinimumSubmittedRounds)
	}
	return false, ""
}

// SubmitWorkflow journals before each write, pins bytes and persists the ID
// before returning. A crash or ambiguous API result blocks another create.
func (c *Client) SubmitWorkflow(ctx context.Context, store *WorkflowStore, plan SubmissionPlan, schema FormSchema, evidence AuditEvidence) (SubmissionResult, error) {
	state, err := store.Load(ctx)
	if err != nil {
		return SubmissionResult{}, err
	}
	if state.Submission.ID > 0 || state.Status != WorkflowDryRunPassed {
		return SubmissionResult{}, fmt.Errorf("workflow is not ready for a new submission")
	}
	if state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain") {
		return SubmissionResult{}, fmt.Errorf("unresolved platform write blocks submission")
	}
	// Short runs stay local only when the evaluation says the platform will not
	// collect them; a real failure below the floor is still submittable.
	if blocked, reason := LocalOnlyDecision(state, plan); blocked {
		return SubmissionResult{}, fmt.Errorf("%s", reason)
	}
	if schema.Frozen || schema.Limited {
		return SubmissionResult{}, fmt.Errorf("platform currently blocks submission; no uploads or creation attempted")
	}
	audit := AuditSubmission(plan, schema, evidence)
	for _, field := range schema.Fields {
		if field.Key == "verify_rubric" {
			if err := VerifyDesignBinding(state, plan); err != nil {
				return SubmissionResult{}, err
			}
			if err := VerifySemanticFields(state, plan, schema); err != nil {
				return SubmissionResult{}, err
			}
			break
		}
	}
	if !audit.Passed || audit.PlanSHA256 != state.Submission.AuditedPlanSHA256 {
		return SubmissionResult{}, fmt.Errorf("submission audit failed or plan changed; zero uploads")
	}
	if state.Submission.SchemaSHA256 != "" || plan.SchemaSHA256 != "" {
		if state.Submission.SchemaSHA256 == "" || plan.SchemaSHA256 == "" || plan.SchemaSHA256 != state.Submission.SchemaSHA256 {
			return SubmissionResult{}, fmt.Errorf("submission schema snapshot is missing or changed; zero uploads")
		}
	}
	for key, hash := range audit.Attachments {
		if state.Verification.Artifacts[key].SHA256 != hash {
			return SubmissionResult{}, fmt.Errorf("attachment differs from frozen task state; zero uploads")
		}
	}
	if state.Precheck.ItemID != audit.PrecheckItemID || state.Precheck.PromptSHA256 != TextSHA256(plan.ExecutionPrompt) {
		return SubmissionResult{}, fmt.Errorf("precheck binding differs from task state; zero uploads")
	}
	if err := c.AuditPrecheck(ctx, plan, &audit); err != nil || !audit.Passed {
		return SubmissionResult{}, fmt.Errorf("live precheck audit failed; zero uploads")
	}
	// Freeze every attachment before the first write, so a later local change
	// cannot cause a partly uploaded collection or alter the audited bytes.
	frozen := make(map[string][]byte, len(plan.Attachments))
	for key, path := range plan.Attachments {
		data, err := os.ReadFile(path)
		if err != nil || TextSHA256(string(data)) != audit.Attachments[key] {
			return SubmissionResult{}, fmt.Errorf("attachment changed during precheck; zero uploads")
		}
		frozen[key] = data
	}
	state, err = BeginWorkflowWrite(ctx, store, state, "submit", "/submissions/create", audit.PlanSHA256)
	if err != nil {
		return SubmissionResult{}, err
	}
	uncertain := func() error {
		// Record an uncertain outcome even when the API context has expired.
		recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = FinishWorkflowWrite(recoveryCtx, store, state, "/submissions/create", 0, false, nil)
		if state.PendingOperation != nil && state.PendingOperation.CreateRequestStarted != nil && !*state.PendingOperation.CreateRequestStarted {
			return fmt.Errorf("attachment outcome uncertain; create was never started, a local reset is eligible and orphan attachments may remain")
		}
		return fmt.Errorf("write outcome uncertain; task is blocked until platform reconciliation")
	}
	data, _ := json.Marshal(plan.Payload)
	var payload map[string]any
	_ = json.Unmarshal(data, &payload)
	keys := make([]string, 0, len(plan.Attachments))
	for key := range plan.Attachments {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		op := state.PendingOperation
		state, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "upload_started", Method: "POST", Endpoint: "/uploads/attachment", ExternalWrite: true, OperationID: op.ID, Outcome: "started", PayloadSHA256: op.PayloadSHA256, ArtifactSHA256: map[string]string{key: audit.Attachments[key]}}, func(s *WorkflowState) error { return nil })
		if err != nil {
			return SubmissionResult{}, fmt.Errorf("journal unavailable; no further writes")
		}
		upload, err := c.uploadAttachmentBytes(ctx, filepath.Base(plan.Attachments[key]), frozen[key])
		if err != nil || upload.URL == "" {
			return SubmissionResult{}, uncertain()
		}
		payload[key] = upload.URL
		observedAt := time.Now().UTC().Format(time.RFC3339Nano)
		state, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "upload_finished", Method: "POST", Endpoint: "/uploads/attachment", ExternalWrite: true, OperationID: op.ID, Outcome: "success", PayloadSHA256: op.PayloadSHA256, ArtifactSHA256: map[string]string{key: audit.Attachments[key]}}, func(s *WorkflowState) error {
			if s.Submission.Uploads == nil {
				s.Submission.Uploads = make(map[string]WorkflowUpload)
			}
			s.Submission.Uploads[key] = WorkflowUpload{
				OperationID: op.ID, ArtifactSHA256: audit.Attachments[key],
				RemoteURLSHA256: TextSHA256(upload.URL), RemoteName: upload.Name,
				Status: "uploaded", ObservedAt: observedAt,
			}
			return nil
		})
		if err != nil {
			return SubmissionResult{}, fmt.Errorf("upload accepted but local journal failed; reload state and reconcile")
		}
	}
	op := state.PendingOperation
	state, err = store.Update(ctx, state.Revision, WorkflowAudit{Action: "create_request_started", Method: "POST", Endpoint: "/submissions/create", ExternalWrite: true, OperationID: op.ID, Outcome: "started", PayloadSHA256: op.PayloadSHA256}, func(next *WorkflowState) error {
		if next.PendingOperation == nil || next.PendingOperation.CreateRequestStarted == nil || *next.PendingOperation.CreateRequestStarted {
			return fmt.Errorf("submission create stage is missing or has already started")
		}
		started := true
		next.PendingOperation.CreateRequestStarted = &started
		return nil
	})
	if err != nil {
		return SubmissionResult{}, fmt.Errorf("create-stage journal failed; create was not attempted, reload before continuing")
	}
	result, err := c.CreateSubmission(ctx, payload, "", schema.Version)
	if err != nil || result.ID <= 0 {
		return SubmissionResult{}, uncertain()
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err = FinishWorkflowWrite(recoveryCtx, store, state, "/submissions/create", result.ID, true, func(s *WorkflowState) error {
		s.Status = WorkflowSubmitted
		s.Submission.ID = result.ID
		s.Submission.Status = result.Status
		return nil
	})
	if err != nil {
		return result, fmt.Errorf("platform created submission %d; journal save failed, do not recreate", result.ID)
	}
	// State is authoritative if updating this convenience copy fails.
	var savedPlan SubmissionPlan
	if err := ReadLocalJSON(state.PlanPath, &savedPlan); err != nil {
		return result, fmt.Errorf("submission %d saved in task state; plan copy unavailable", result.ID)
	}
	savedPlan.ExistingSubmissionID = result.ID
	if err := atomicWorkflowJSON(state.PlanPath, savedPlan); err != nil {
		return result, fmt.Errorf("submission %d saved in task state; plan copy update failed", result.ID)
	}
	return result, nil
}

func SaveLocalJSON(path string, value any) error {
	return atomicWorkflowJSON(filepath.Clean(path), value)
}
