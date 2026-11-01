package precheckclient

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type WorkflowStatus string

const (
	WorkflowInitialized       WorkflowStatus = "initialized"
	WorkflowProjectSelected   WorkflowStatus = "project_selected"
	WorkflowPrecheckDraft     WorkflowStatus = "precheck_draft"
	WorkflowPrecheckSubmitted WorkflowStatus = "precheck_submitted"
	WorkflowPrecheckRejected  WorkflowStatus = "precheck_rejected"
	WorkflowPrecheckPassed    WorkflowStatus = "precheck_passed"
	WorkflowTraeRunning       WorkflowStatus = "trae_running"
	WorkflowSessionCaptured   WorkflowStatus = "session_captured"
	WorkflowSessionVerified   WorkflowStatus = "session_verified"
	WorkflowArtifactsVerified WorkflowStatus = "artifacts_verified"
	WorkflowDryRunPassed      WorkflowStatus = "dry_run_passed"
	WorkflowSubmitting        WorkflowStatus = "submitting"
	WorkflowSubmitted         WorkflowStatus = "submitted"
	WorkflowQCPending         WorkflowStatus = "qc_pending"
	WorkflowQCRejected        WorkflowStatus = "qc_rejected"
	WorkflowQCAccepted        WorkflowStatus = "qc_accepted"
	WorkflowClosed            WorkflowStatus = "closed"
	// WorkflowLocalOnly is the terminal state for a finished run the platform
	// will not accept: it is kept, reviewed and edited on this machine only.
	WorkflowLocalOnly WorkflowStatus = "local_only"
)

type WorkflowProject struct {
	RepoURL        string `json:"repo_url,omitempty"`
	BaselineCommit string `json:"baseline_commit,omitempty"`
	Language       string `json:"language,omitempty"`
}

type WorkflowPrecheck struct {
	BatchID      string `json:"batch_id,omitempty"`
	ItemID       int    `json:"item_id,omitempty"`
	Status       string `json:"status,omitempty"`
	PromptSHA256 string `json:"prompt_sha256,omitempty"`
}

type WorkflowTrae struct {
	ExecutionMode      string `json:"execution_mode,omitempty"`
	ExecutionStartedAt string `json:"execution_started_at,omitempty"`
	Model              string `json:"model,omitempty"`
	Version            string `json:"version,omitempty"`
	SessionTokenSHA256 string `json:"session_token_sha256,omitempty"`
	// SessionVerificationSkipped records an explicit operator request to save
	// the original string without matching it against Trae logs. It never means
	// the session was verified; the UI and the Codex handoff must say so.
	SessionVerificationSkipped bool   `json:"session_verification_skipped,omitempty"`
	EffectiveRounds            *int   `json:"effective_rounds,omitempty"`
	SessionAuditPath           string `json:"session_audit_path,omitempty"`
	SessionAuditSHA256         string `json:"session_audit_sha256,omitempty"`
}

type WorkflowArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type WorkflowVerification struct {
	DesignPath       string                      `json:"design_path,omitempty"`
	DesignSHA256     string                      `json:"design_sha256,omitempty"`
	Artifacts        map[string]WorkflowArtifact `json:"artifacts,omitempty"`
	RubricReportPath string                      `json:"rubric_report_path,omitempty"`
	RubricSHA256     string                      `json:"rubric_sha256,omitempty"`
	Preflight        WorkflowPreflight           `json:"preflight,omitempty"`
}

type WorkflowPreflight struct {
	Path         string `json:"path,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	InputsSHA256 string `json:"inputs_sha256,omitempty"`
	Profile      string `json:"profile,omitempty"`
	Passed       bool   `json:"passed"`
	ObservedAt   string `json:"observed_at,omitempty"`
}

type WorkflowSubmission struct {
	ID                int                       `json:"id,omitempty"`
	Status            string                    `json:"status,omitempty"`
	SchemaPath        string                    `json:"schema_path,omitempty"`
	SchemaSHA256      string                    `json:"schema_sha256,omitempty"`
	AuditPath         string                    `json:"audit_path,omitempty"`
	AuditSHA256       string                    `json:"audit_sha256,omitempty"`
	AuditedPlanSHA256 string                    `json:"audited_plan_sha256,omitempty"`
	ReceiptPath       string                    `json:"receipt_path,omitempty"`
	ReceiptSHA256     string                    `json:"receipt_sha256,omitempty"`
	ObservedAt        string                    `json:"observed_at,omitempty"`
	Source            string                    `json:"source,omitempty"`
	Uploads           map[string]WorkflowUpload `json:"uploads,omitempty"`
}

// WorkflowUpload records the locally auditable result of each attachment
// upload without persisting the remote URL or other server details.
type WorkflowUpload struct {
	OperationID     string `json:"operation_id"`
	ArtifactSHA256  string `json:"artifact_sha256"`
	RemoteURLSHA256 string `json:"remote_url_sha256"`
	RemoteName      string `json:"remote_name,omitempty"`
	Status          string `json:"status"`
	ObservedAt      string `json:"observed_at"`
}

// PendingOperation survives a timeout or crash. A nonterminal outcome blocks
// creating another write until the original result has been reconciled.
type WorkflowOperation struct {
	ID                    string            `json:"id"`
	Kind                  string            `json:"kind"`
	PayloadSHA256         string            `json:"payload_sha256"`
	StartedAt             string            `json:"started_at"`
	Outcome               string            `json:"outcome"`
	PrecheckBefore        *WorkflowPrecheck `json:"precheck_before,omitempty"`
	PriorPrecheckItemIDs  []int             `json:"prior_precheck_item_ids,omitempty"`
	PrecheckSnapshotTaken bool              `json:"precheck_snapshot_taken,omitempty"`
	CreateRequestStarted  *bool             `json:"create_request_started,omitempty"`
}

type WorkflowState struct {
	SchemaVersion    int                  `json:"schema_version"`
	TaskID           string               `json:"task_id"`
	Status           WorkflowStatus       `json:"status"`
	Revision         int64                `json:"revision"`
	UpdatedAt        string               `json:"updated_at"`
	LastEventSHA256  string               `json:"last_event_sha256"`
	PlanPath         string               `json:"plan_path,omitempty"`
	PlanSHA256       string               `json:"plan_sha256,omitempty"`
	Workspace        string               `json:"workspace"`
	Project          WorkflowProject      `json:"project"`
	Precheck         WorkflowPrecheck     `json:"precheck"`
	Trae             WorkflowTrae         `json:"trae"`
	Verification     WorkflowVerification `json:"verification"`
	Submission       WorkflowSubmission   `json:"submission"`
	PendingOperation *WorkflowOperation   `json:"pending_operation,omitempty"`
	// Archived shelves a task without touching its workflow status: any stage
	// can be archived and restored, and every transition still applies while
	// it is shelved. It is not part of the audited materials.
	Archived bool `json:"archived,omitempty"`
}

// WorkflowAudit intentionally has no free-text error, payload or token field.
// API errors belong in private diagnostics; journal entries use stable codes.
type WorkflowAudit struct {
	Action         string            `json:"action"`
	Method         string            `json:"method,omitempty"`
	Endpoint       string            `json:"endpoint,omitempty"`
	OperationID    string            `json:"operation_id,omitempty"`
	Outcome        string            `json:"outcome,omitempty"`
	ErrorCode      string            `json:"error_code,omitempty"`
	ExternalWrite  bool              `json:"external_write"`
	ResultID       int               `json:"result_id,omitempty"`
	PayloadSHA256  string            `json:"payload_sha256,omitempty"`
	ArtifactSHA256 map[string]string `json:"artifact_sha256,omitempty"`
}

type workflowEvent struct {
	ID           string         `json:"event_id"`
	At           string         `json:"at"`
	PreviousHash string         `json:"previous_sha256,omitempty"`
	Hash         string         `json:"sha256"`
	From         WorkflowStatus `json:"from,omitempty"`
	To           WorkflowStatus `json:"to"`
	Audit        WorkflowAudit  `json:"audit"`
	State        WorkflowState  `json:"state"`
}

type WorkflowStore struct {
	Path           string
	journalWriter  func(*os.File, []byte) error
	snapshotWriter func(string, any) error
}

func NewWorkflowStore(path string) *WorkflowStore { return &WorkflowStore{Path: path} }

var workflowIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var workflowEndpoint = regexp.MustCompile(`^/[a-zA-Z0-9_/-]+$`)
var workflowHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

var workflowTransitions = map[WorkflowStatus][]WorkflowStatus{
	WorkflowInitialized:       {WorkflowProjectSelected, WorkflowLocalOnly},
	WorkflowProjectSelected:   {WorkflowPrecheckDraft, WorkflowLocalOnly},
	WorkflowPrecheckDraft:     {WorkflowPrecheckSubmitted, WorkflowLocalOnly},
	WorkflowPrecheckSubmitted: {WorkflowPrecheckPassed, WorkflowPrecheckRejected, WorkflowLocalOnly},
	WorkflowPrecheckRejected:  {WorkflowPrecheckDraft, WorkflowPrecheckSubmitted, WorkflowLocalOnly},
	WorkflowPrecheckPassed:    {WorkflowTraeRunning, WorkflowLocalOnly},
	WorkflowTraeRunning:       {WorkflowSessionCaptured, WorkflowLocalOnly},
	WorkflowSessionCaptured:   {WorkflowSessionVerified, WorkflowTraeRunning, WorkflowLocalOnly},
	WorkflowSessionVerified:   {WorkflowArtifactsVerified, WorkflowTraeRunning, WorkflowLocalOnly},
	WorkflowArtifactsVerified: {WorkflowDryRunPassed, WorkflowSessionVerified, WorkflowLocalOnly},
	WorkflowDryRunPassed:      {WorkflowSubmitting, WorkflowArtifactsVerified, WorkflowLocalOnly},
	WorkflowSubmitting:        {WorkflowSubmitted, WorkflowDryRunPassed},
	WorkflowSubmitted:         {WorkflowQCPending, WorkflowQCRejected, WorkflowQCAccepted, WorkflowClosed},
	WorkflowQCPending:         {WorkflowQCRejected, WorkflowQCAccepted},
	WorkflowQCRejected:        {WorkflowQCPending, WorkflowClosed},
	WorkflowQCAccepted:        {WorkflowClosed},
	WorkflowClosed:            {},
	// A local-only run is terminal for the platform but stays editable: reopening
	// returns it to the stage it can actually resume from.
	WorkflowLocalOnly: {WorkflowSessionCaptured, WorkflowPrecheckPassed, WorkflowProjectSelected},
}

func (s *WorkflowStore) Init(ctx context.Context, taskID, workspace string) (WorkflowState, error) {
	var result WorkflowState
	err := s.locked(ctx, func() error {
		if _, err := os.Lstat(s.Path); err == nil {
			return fmt.Errorf("task state already exists; use status before resuming")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := os.Stat(s.eventsPath()); err == nil {
			return fmt.Errorf("task journal already exists; recover it before initializing")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		absolute, err := filepath.Abs(workspace)
		if err != nil {
			return err
		}
		absolute, err = filepath.EvalSymlinks(absolute)
		if err != nil {
			return fmt.Errorf("task workspace is unavailable")
		}
		if info, err := os.Stat(absolute); err != nil || !info.IsDir() {
			return fmt.Errorf("task workspace must be a directory")
		}
		state := WorkflowState{SchemaVersion: 1, TaskID: taskID, Status: WorkflowInitialized, Workspace: absolute}
		result, err = s.commit(WorkflowState{}, state, WorkflowAudit{Action: "init", Outcome: "success"})
		return err
	})
	return result, err
}

func (s *WorkflowStore) Load(ctx context.Context) (WorkflowState, error) {
	// An unchanged journal is served from the verified-state cache instead of
	// replaying and re-hashing the whole chain on every read.
	if state, ok := loadVerifiedFromCache(s.Path); ok {
		return state, nil
	}
	var result WorkflowState
	var stamp workflowJournalStamp
	cacheable := false
	err := s.locked(ctx, func() error {
		// Both stamps are read under the same task lock that writers take, so a
		// journal appended after verification cannot be cached under the newer
		// stamp; the mismatch simply leaves the state uncached.
		before, beforeOK := workflowStoreStamp(s.Path)
		var loadErr error
		result, loadErr = s.load()
		if loadErr != nil {
			return loadErr
		}
		after, afterOK := workflowStoreStamp(s.Path)
		if beforeOK && afterOK && before == after {
			stamp, cacheable = after, true
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	if cacheable {
		storeVerifiedInCache(s.Path, stamp, result)
	}
	return result, nil
}

// ImportHistorical links an existing saved receipt without replaying past
// transitions or implying that a fresh platform query was performed.
func (s *WorkflowStore) ImportHistorical(ctx context.Context, taskID, workspace, planPath, receiptPath string) (WorkflowState, error) {
	var result WorkflowState
	err := s.locked(ctx, func() error {
		for _, path := range []string{s.Path, s.eventsPath()} {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("historical import requires a new task state and journal")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		absoluteWorkspace, err := resolveWorkflowPath(workspace)
		if err != nil {
			return fmt.Errorf("historical workspace is unavailable")
		}
		if info, err := os.Stat(absoluteWorkspace); err != nil || !info.IsDir() {
			return fmt.Errorf("historical workspace must be a directory")
		}
		absolutePlan, err := resolveWorkflowPath(planPath)
		if err != nil {
			return fmt.Errorf("historical plan is unavailable")
		}
		info, err := os.Stat(absolutePlan)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("historical plan must be a private regular file")
		}
		planBytes, err := os.ReadFile(absolutePlan)
		if err != nil {
			return fmt.Errorf("cannot read historical plan")
		}
		var plan SubmissionPlan
		if err := json.Unmarshal(planBytes, &plan); err != nil || plan.Payload == nil || strings.TrimSpace(plan.ExecutionPrompt) == "" {
			return fmt.Errorf("invalid historical submission plan")
		}
		if err := validateSubmissionPayload(plan.Payload); err != nil {
			return fmt.Errorf("historical plan contains forbidden payload fields")
		}
		absoluteReceipt, err := resolveWorkflowPath(receiptPath)
		if err != nil {
			return fmt.Errorf("historical receipt is unavailable")
		}
		receiptBytes, err := os.ReadFile(absoluteReceipt)
		if err != nil {
			return fmt.Errorf("cannot read historical receipt")
		}
		var receipt struct {
			UpdatedAt          string `json:"updated_at"`
			ServerVerification struct {
				SubmissionID          int    `json:"new_submission_id"`
				Status                string `json:"status"`
				SessionSHA256         string `json:"session_sha256"`
				EffectiveRounds       int    `json:"effective_rounds"`
				PrecheckItemID        int    `json:"precheck_item_id"`
				SessionExactMatch     bool   `json:"session_exact_match"`
				PromptExactMatch      bool   `json:"prompt_exact_match"`
				ArtifactShotSaved     bool   `json:"artifact_shot_saved"`
				TraceSaved            bool   `json:"trace_saved"`
				RemarksInServerValues bool   `json:"remarks_in_server_values"`
			} `json:"server_verification"`
		}
		if err := json.Unmarshal(receiptBytes, &receipt); err != nil {
			return fmt.Errorf("invalid historical receipt")
		}
		remote := receipt.ServerVerification
		if remote.Status != "PENDING_FIX" || remote.SubmissionID <= 0 || remote.PrecheckItemID <= 0 || !remote.SessionExactMatch || !remote.PromptExactMatch || !remote.ArtifactShotSaved || !remote.TraceSaved || remote.RemarksInServerValues {
			return fmt.Errorf("historical receipt does not contain a supported verified submission")
		}
		if _, err := time.Parse(time.RFC3339Nano, receipt.UpdatedAt); err != nil {
			return fmt.Errorf("historical receipt requires its original verification timestamp")
		}
		itemID, itemErr := planInteger(plan.Payload["precheck_item_id"])
		rounds, roundsErr := planInteger(plan.Payload["effective_rounds"])
		token, _ := plan.Payload["trae_session_id"].(string)
		if err := ValidateTraeSessionToken(token); err != nil {
			return fmt.Errorf("historical plan does not contain a complete session token")
		}
		if itemErr != nil || roundsErr != nil || rounds < 0 || itemID != remote.PrecheckItemID || rounds != remote.EffectiveRounds || plan.ExistingSubmissionID != remote.SubmissionID || bytesSHA256([]byte(token)) != remote.SessionSHA256 {
			return fmt.Errorf("historical plan differs from the saved server receipt")
		}
		repoURL, _ := plan.Payload["repo_url"].(string)
		state := WorkflowState{
			SchemaVersion: 1, TaskID: taskID, Status: WorkflowQCRejected, Workspace: absoluteWorkspace,
			PlanPath: absolutePlan, PlanSHA256: bytesSHA256(planBytes),
			Project:      WorkflowProject{RepoURL: repoURL},
			Precheck:     WorkflowPrecheck{ItemID: itemID, PromptSHA256: bytesSHA256([]byte(plan.ExecutionPrompt))},
			Trae:         WorkflowTrae{SessionTokenSHA256: remote.SessionSHA256, EffectiveRounds: &rounds},
			Submission:   WorkflowSubmission{ID: remote.SubmissionID, Status: remote.Status, ReceiptPath: absoluteReceipt, ReceiptSHA256: bytesSHA256(receiptBytes), ObservedAt: receipt.UpdatedAt, Source: "local_saved_receipt"},
			Verification: WorkflowVerification{Artifacts: map[string]WorkflowArtifact{}},
		}
		for key, path := range plan.Attachments {
			if !workflowIdentifier.MatchString(key) {
				return fmt.Errorf("invalid historical artifact identity")
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(absolutePlan), path)
			}
			artifact, err := HashWorkflowArtifact(path)
			if err != nil {
				return fmt.Errorf("historical attachment %q is unavailable", key)
			}
			state.Verification.Artifacts[key] = artifact
		}
		if _, ok := state.Verification.Artifacts["x_artifact_shot"]; !ok {
			return fmt.Errorf("historical screenshot attachment is missing")
		}
		if _, ok := state.Verification.Artifacts["x_trae_trace"]; !ok {
			return fmt.Errorf("historical trace attachment is missing")
		}
		result, err = s.commit(WorkflowState{}, state, WorkflowAudit{Action: "historical_import", Outcome: "success", ResultID: remote.SubmissionID, PayloadSHA256: state.PlanSHA256})
		return err
	})
	return result, err
}

func resolveWorkflowPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}

// Update uses an expected revision so concurrent workers cannot overwrite a
// newer task snapshot. The callback executes while holding the workflow lock.
func (s *WorkflowStore) Update(ctx context.Context, expectedRevision int64, audit WorkflowAudit, mutate func(*WorkflowState) error) (WorkflowState, error) {
	var result WorkflowState
	err := s.locked(ctx, func() error {
		previous, err := s.load()
		if err != nil {
			return err
		}
		if previous.Revision != expectedRevision {
			return fmt.Errorf("task revision changed; reload before retrying")
		}
		data, _ := json.Marshal(previous)
		var next WorkflowState
		_ = json.Unmarshal(data, &next)
		if mutate == nil {
			return fmt.Errorf("workflow update requires a mutation")
		}
		if err := mutate(&next); err != nil {
			return err
		}
		if next.TaskID != previous.TaskID || next.Workspace != previous.Workspace || next.SchemaVersion != previous.SchemaVersion || next.Revision != previous.Revision || next.LastEventSHA256 != previous.LastEventSHA256 {
			return fmt.Errorf("immutable workflow identity or revision was changed")
		}
		if previous.Verification.DesignPath != "" && (next.Verification.DesignPath != previous.Verification.DesignPath || next.Verification.DesignSHA256 != previous.Verification.DesignSHA256) {
			return fmt.Errorf("frozen design identity cannot be changed")
		}
		if next.Status != previous.Status && !allowedTransition(previous.Status, next.Status) {
			return fmt.Errorf("invalid workflow transition; reload the task and use the next supported operation")
		}
		if previous.PendingOperation != nil && next.PendingOperation != nil && previous.PendingOperation.ID == next.PendingOperation.ID {
			before, after := previous.PendingOperation.CreateRequestStarted, next.PendingOperation.CreateRequestStarted
			if before == nil && after != nil {
				return fmt.Errorf("legacy operation has no create-stage evidence; cannot infer it locally")
			}
			if before != nil && (after == nil || (*before && !*after)) {
				return fmt.Errorf("durable create-stage evidence cannot be cleared or reversed")
			}
			if before != nil && !*before && after != nil && *after && (previous.PendingOperation.Kind != "submit" || previous.PendingOperation.Outcome != "started" || audit.Action != "create_request_started" || audit.OperationID != previous.PendingOperation.ID || !audit.ExternalWrite || audit.Method != "POST" || audit.Endpoint != "/submissions/create" || audit.Outcome != "started") {
				return fmt.Errorf("create-stage evidence requires its write-ahead journal event")
			}
		}
		if previous.PendingOperation != nil && (previous.PendingOperation.Outcome == "started" || previous.PendingOperation.Outcome == "uncertain") {
			if next.PendingOperation != nil && next.PendingOperation.ID != previous.PendingOperation.ID {
				return fmt.Errorf("unresolved external write blocks another operation")
			}
			if next.PendingOperation == nil && (audit.OperationID != previous.PendingOperation.ID || (audit.Action != "reconcile" && (previous.PendingOperation.Outcome != "started" || audit.Outcome != "success"))) {
				return fmt.Errorf("unresolved external write cannot be cleared without a confirmed result")
			}
			if next.PendingOperation != nil && previous.PendingOperation.Outcome == "uncertain" && next.PendingOperation.Outcome != "uncertain" && audit.Action != "reconcile" {
				return fmt.Errorf("uncertain operation requires explicit platform reconciliation")
			}
			if next.PendingOperation != nil && (next.PendingOperation.Kind != previous.PendingOperation.Kind || next.PendingOperation.PayloadSHA256 != previous.PendingOperation.PayloadSHA256 || next.PendingOperation.StartedAt != previous.PendingOperation.StartedAt || !reflect.DeepEqual(next.PendingOperation.PrecheckBefore, previous.PendingOperation.PrecheckBefore) || !reflect.DeepEqual(next.PendingOperation.PriorPrecheckItemIDs, previous.PendingOperation.PriorPrecheckItemIDs) || next.PendingOperation.PrecheckSnapshotTaken != previous.PendingOperation.PrecheckSnapshotTaken) {
				return fmt.Errorf("pending operation identity and payload are immutable")
			}
			if next.Status == WorkflowDryRunPassed && previous.Status == WorkflowSubmitting && audit.Action != "reconcile" {
				return fmt.Errorf("uncertain submission requires reconciliation before retrying")
			}
		}
		if frozenWorkflowMaterials(previous.Status) && next.Status != WorkflowArtifactsVerified && !sameWorkflowMaterials(previous, next) {
			return fmt.Errorf("audited task materials are frozen; invalidate the audit before rebinding")
		}
		result, err = s.commit(previous, next, audit)
		return err
	})
	return result, err
}

func (s *WorkflowStore) Advance(ctx context.Context, expectedRevision int64, next WorkflowStatus, audit WorkflowAudit) (WorkflowState, error) {
	return s.Update(ctx, expectedRevision, audit, func(state *WorkflowState) error {
		if state.Status == next {
			return fmt.Errorf("task is already in requested status")
		}
		state.Status = next
		return nil
	})
}

func (s *WorkflowStore) BindPlan(ctx context.Context, expectedRevision int64, planPath string) (WorkflowState, error) {
	absolute, err := filepath.Abs(planPath)
	if err != nil {
		return WorkflowState{}, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return WorkflowState{}, fmt.Errorf("plan is unavailable")
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return WorkflowState{}, fmt.Errorf("plan must be a readable regular file with permission 0600")
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return WorkflowState{}, fmt.Errorf("cannot read submission plan")
	}
	var plan SubmissionPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return WorkflowState{}, fmt.Errorf("cannot decode submission plan")
	}
	if plan.Payload == nil || strings.TrimSpace(plan.ExecutionPrompt) == "" {
		return WorkflowState{}, fmt.Errorf("plan has no payload or execution prompt")
	}
	planHash := bytesSHA256(data)
	return s.Update(ctx, expectedRevision, WorkflowAudit{Action: "bind_plan", Outcome: "success", PayloadSHA256: planHash}, func(state *WorkflowState) error {
		if state.Verification.DesignPath != "" {
			if err := VerifyDesignBinding(*state, plan); err != nil {
				return err
			}
		}
		if state.Submission.SchemaSHA256 != "" || plan.SchemaSHA256 != "" {
			if state.Submission.SchemaSHA256 == "" || plan.SchemaSHA256 == "" || plan.SchemaSHA256 != state.Submission.SchemaSHA256 {
				return fmt.Errorf("plan schema snapshot differs from task schema snapshot")
			}
		}
		repoURL, _ := plan.Payload["repo_url"].(string)
		if state.Project.RepoURL != "" && repoURL != state.Project.RepoURL {
			return fmt.Errorf("plan repository differs from task state")
		}
		for key, expected := range map[string]string{"commit_ref": state.Project.BaselineCommit, "primary_language": state.Project.Language, "seed_model": state.Trae.Model} {
			if expected != "" && plan.Payload[key] != expected {
				return fmt.Errorf("plan field %q differs from task state", key)
			}
		}
		itemID, err := planInteger(plan.Payload["precheck_item_id"])
		if err != nil || itemID <= 0 {
			return fmt.Errorf("plan has an invalid precheck identity")
		}
		if state.Precheck.ItemID > 0 && state.Precheck.ItemID != itemID {
			return fmt.Errorf("plan precheck differs from task state")
		}
		promptHash := bytesSHA256([]byte(plan.ExecutionPrompt))
		if state.Precheck.PromptSHA256 != "" && state.Precheck.PromptSHA256 != promptHash {
			return fmt.Errorf("plan prompt differs from bound precheck")
		}
		if state.Submission.ID > 0 && plan.ExistingSubmissionID != state.Submission.ID {
			return fmt.Errorf("plan submission identity differs from task state")
		}
		if state.PendingOperation != nil && (state.PendingOperation.Outcome == "started" || state.PendingOperation.Outcome == "uncertain") && state.PlanSHA256 != planHash {
			return fmt.Errorf("cannot replace a plan while an external write is unresolved")
		}
		state.PlanPath, state.PlanSHA256 = absolute, planHash
		state.Project.RepoURL = repoURL
		state.Precheck.ItemID, state.Precheck.PromptSHA256 = itemID, promptHash
		if token, _ := plan.Payload["trae_session_id"].(string); token != "" {
			if err := ValidateTraeSessionToken(token); err != nil {
				return fmt.Errorf("plan session is not a verified complete token")
			}
			state.Trae.SessionTokenSHA256 = bytesSHA256([]byte(token))
		}
		if rounds, ok := plan.Payload["effective_rounds"]; ok {
			n, err := planInteger(rounds)
			if err != nil || n < 0 {
				return fmt.Errorf("plan effective rounds must be a nonnegative integer")
			}
			state.Trae.EffectiveRounds = &n
		}
		artifacts := make(map[string]WorkflowArtifact, len(plan.Attachments))
		for key, path := range plan.Attachments {
			if !workflowIdentifier.MatchString(key) {
				return fmt.Errorf("unsupported attachment identity")
			}
			if !filepath.IsAbs(path) {
				path = filepath.Join(filepath.Dir(absolute), path)
			}
			artifact, err := HashWorkflowArtifact(path)
			if err != nil {
				return fmt.Errorf("cannot bind attachment %q", key)
			}
			artifacts[key] = artifact
		}
		state.Verification.Artifacts = artifacts
		if plan.ExistingSubmissionID > 0 {
			state.Submission.ID = plan.ExistingSubmissionID
		}
		return nil
	})
}

func HashWorkflowArtifact(path string) (WorkflowArtifact, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return WorkflowArtifact{}, err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return WorkflowArtifact{}, fmt.Errorf("artifact is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return WorkflowArtifact{}, fmt.Errorf("artifact must be a nonempty regular file")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, file)
	if err != nil {
		return WorkflowArtifact{}, fmt.Errorf("cannot hash artifact")
	}
	return WorkflowArtifact{Path: absolute, SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: count}, nil
}

func planInteger(value any) (int, error) {
	n, ok := value.(float64)
	if !ok || n != float64(int(n)) {
		return 0, fmt.Errorf("not an integer")
	}
	return int(n), nil
}

func allowedTransition(from, to WorkflowStatus) bool {
	for _, status := range workflowTransitions[from] {
		if status == to {
			return true
		}
	}
	return false
}

func frozenWorkflowMaterials(status WorkflowStatus) bool {
	switch status {
	case WorkflowDryRunPassed, WorkflowSubmitting, WorkflowSubmitted, WorkflowQCPending, WorkflowQCRejected, WorkflowQCAccepted, WorkflowClosed:
		return true
	}
	return false
}

func sameWorkflowMaterials(a, b WorkflowState) bool {
	return a.PlanPath == b.PlanPath && a.PlanSHA256 == b.PlanSHA256 &&
		reflect.DeepEqual(a.Project, b.Project) &&
		a.Precheck.ItemID == b.Precheck.ItemID && a.Precheck.PromptSHA256 == b.Precheck.PromptSHA256 &&
		reflect.DeepEqual(a.Trae, b.Trae) && reflect.DeepEqual(a.Verification, b.Verification) &&
		a.Submission.SchemaPath == b.Submission.SchemaPath &&
		a.Submission.SchemaSHA256 == b.Submission.SchemaSHA256 &&
		a.Submission.AuditPath == b.Submission.AuditPath &&
		a.Submission.AuditSHA256 == b.Submission.AuditSHA256 &&
		a.Submission.AuditedPlanSHA256 == b.Submission.AuditedPlanSHA256
}

func validateWorkflowState(state WorkflowState) error {
	if state.SchemaVersion != 1 || !workflowIdentifier.MatchString(state.TaskID) || !filepath.IsAbs(state.Workspace) {
		return fmt.Errorf("invalid workflow schema or identity")
	}
	if _, ok := workflowTransitions[state.Status]; !ok {
		return fmt.Errorf("unsupported workflow status")
	}
	if state.Trae.ExecutionMode != "" && state.Trae.ExecutionMode != "manual" {
		return fmt.Errorf("unsupported execution mode")
	}
	if state.Trae.ExecutionStartedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, state.Trae.ExecutionStartedAt); err != nil {
			return fmt.Errorf("invalid execution timestamp")
		}
	}
	if state.Verification.DesignPath != "" && (!filepath.IsAbs(state.Verification.DesignPath) || !workflowHash.MatchString(state.Verification.DesignSHA256)) {
		return fmt.Errorf("invalid design material binding")
	}
	if state.Project.RepoURL != "" {
		u, err := url.ParseRequestURI(state.Project.RepoURL)
		if err != nil || u.Host == "" || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("repository URL must not contain credentials or query parameters")
		}
	}
	for _, value := range []string{state.Project.BaselineCommit, state.Project.Language, state.Precheck.BatchID, state.Precheck.Status, state.Trae.Model, state.Trae.Version, state.Submission.Status} {
		if len(value) > 128 || strings.ContainsAny(value, "\r\n\x00") || sessionTokenPrefix.MatchString(value) || strings.Contains(value, "swe_qa_session") || strings.Contains(value, "swe_qa_csrf") {
			return fmt.Errorf("workflow metadata must not contain raw telemetry, cookies or multiline text")
		}
	}
	for _, hash := range []string{state.PlanSHA256, state.Precheck.PromptSHA256, state.Trae.SessionTokenSHA256, state.Trae.SessionAuditSHA256, state.Verification.RubricSHA256, state.Verification.Preflight.SHA256, state.Verification.Preflight.InputsSHA256, state.Submission.SchemaSHA256, state.Submission.AuditSHA256, state.Submission.AuditedPlanSHA256, state.Submission.ReceiptSHA256} {
		if hash != "" && !workflowHash.MatchString(hash) {
			return fmt.Errorf("invalid workflow SHA-256")
		}
	}
	if state.Verification.Preflight.Path != "" {
		if !filepath.IsAbs(state.Verification.Preflight.Path) || state.Verification.Preflight.Profile == "" || state.Verification.Preflight.ObservedAt == "" {
			return fmt.Errorf("invalid workflow preflight metadata")
		}
		if _, err := time.Parse(time.RFC3339Nano, state.Verification.Preflight.ObservedAt); err != nil {
			return fmt.Errorf("invalid workflow preflight timestamp")
		}
	}
	if state.Precheck.ItemID < 0 || state.Submission.ID < 0 || (state.Trae.EffectiveRounds != nil && *state.Trae.EffectiveRounds < 0) {
		return fmt.Errorf("invalid workflow record ID or round count")
	}
	for key, artifact := range state.Verification.Artifacts {
		if !workflowIdentifier.MatchString(key) || !filepath.IsAbs(artifact.Path) || !workflowHash.MatchString(artifact.SHA256) || artifact.Bytes <= 0 {
			return fmt.Errorf("invalid workflow artifact metadata")
		}
	}
	for key, upload := range state.Submission.Uploads {
		if !workflowIdentifier.MatchString(key) || !workflowIdentifier.MatchString(upload.OperationID) ||
			!workflowHash.MatchString(upload.ArtifactSHA256) || !workflowHash.MatchString(upload.RemoteURLSHA256) ||
			!validUploadStatus(upload.Status) {
			return fmt.Errorf("invalid workflow upload metadata")
		}
		if _, err := time.Parse(time.RFC3339Nano, upload.ObservedAt); err != nil {
			return fmt.Errorf("invalid workflow upload timestamp")
		}
	}
	if op := state.PendingOperation; op != nil {
		if !workflowIdentifier.MatchString(op.ID) || !workflowIdentifier.MatchString(op.Kind) || !workflowHash.MatchString(op.PayloadSHA256) || !validOutcome(op.Outcome) {
			return fmt.Errorf("invalid pending operation metadata")
		}
		if _, err := time.Parse(time.RFC3339Nano, op.StartedAt); err != nil {
			return fmt.Errorf("invalid pending operation timestamp")
		}
		if op.CreateRequestStarted != nil && op.Kind != "submit" {
			return fmt.Errorf("create-stage evidence only belongs to submission operations")
		}
		if before := op.PrecheckBefore; before != nil {
			if before.ItemID <= 0 || before.BatchID == "" || len(before.BatchID) > 128 || strings.ContainsAny(before.BatchID, "\r\n\x00") || !workflowHash.MatchString(before.PromptSHA256) {
				return fmt.Errorf("invalid precheck recovery identity")
			}
		}
		seen := make(map[int]bool)
		for _, id := range op.PriorPrecheckItemIDs {
			if id <= 0 || seen[id] {
				return fmt.Errorf("invalid precheck recovery snapshot")
			}
			seen[id] = true
		}
	}
	return nil
}

func validUploadStatus(status string) bool {
	return status == "uploaded" || status == "uncertain"
}

func validOutcome(outcome string) bool {
	switch outcome {
	case "started", "success", "failure", "uncertain", "reconciled":
		return true
	}
	return false
}

func validateWorkflowAudit(audit WorkflowAudit) error {
	if !workflowIdentifier.MatchString(audit.Action) || (audit.OperationID != "" && !workflowIdentifier.MatchString(audit.OperationID)) || (audit.ErrorCode != "" && !workflowIdentifier.MatchString(audit.ErrorCode)) {
		return fmt.Errorf("journal only accepts structured identifiers, never raw errors or tokens")
	}
	if audit.Outcome != "" && !validOutcome(audit.Outcome) {
		return fmt.Errorf("invalid journal outcome")
	}
	if audit.Endpoint != "" && !workflowEndpoint.MatchString(audit.Endpoint) {
		return fmt.Errorf("journal endpoint must not contain URLs, query strings or secrets")
	}
	if audit.Method != "" && audit.Method != "GET" && audit.Method != "HEAD" && audit.Method != "OPTIONS" && audit.Method != "POST" && audit.Method != "PUT" && audit.Method != "PATCH" && audit.Method != "DELETE" {
		return fmt.Errorf("invalid journal method")
	}
	if audit.ExternalWrite && (audit.Method == "" || audit.Endpoint == "" || audit.OperationID == "" || audit.PayloadSHA256 == "") {
		return fmt.Errorf("external writes require endpoint, operation ID and payload hash")
	}
	if audit.ResultID < 0 || (audit.PayloadSHA256 != "" && !workflowHash.MatchString(audit.PayloadSHA256)) {
		return fmt.Errorf("invalid journal result or payload hash")
	}
	for key, hash := range audit.ArtifactSHA256 {
		if !workflowIdentifier.MatchString(key) || !workflowHash.MatchString(hash) {
			return fmt.Errorf("invalid journal artifact hash")
		}
	}
	return nil
}

func (s *WorkflowStore) commit(previous, next WorkflowState, audit WorkflowAudit) (WorkflowState, error) {
	if err := validateWorkflowState(next); err != nil {
		return WorkflowState{}, err
	}
	if err := validateWorkflowAudit(audit); err != nil {
		return WorkflowState{}, err
	}
	next.Revision = previous.Revision + 1
	next.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	next.LastEventSHA256 = ""
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return WorkflowState{}, err
	}
	event := workflowEvent{ID: hex.EncodeToString(random[:]), At: next.UpdatedAt, PreviousHash: previous.LastEventSHA256, From: previous.Status, To: next.Status, Audit: audit, State: next}
	encoded, err := json.Marshal(event)
	if err != nil {
		return WorkflowState{}, err
	}
	event.Hash = bytesSHA256(encoded)
	next.LastEventSHA256 = event.Hash
	encoded, _ = json.Marshal(event)
	file, err := os.OpenFile(s.eventsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return WorkflowState{}, fmt.Errorf("cannot open workflow journal")
	}
	if err = file.Chmod(0600); err == nil {
		writer := s.journalWriter
		if writer == nil {
			writer = writeWorkflowJournal
		}
		err = writer(file, append(encoded, '\n'))
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return WorkflowState{}, fmt.Errorf("workflow journal write was interrupted; reload before continuing")
	}
	if err := s.saveSnapshot(next); err != nil {
		return WorkflowState{}, fmt.Errorf("journal committed but snapshot update failed; reload to recover")
	}
	return next, nil
}

func (s *WorkflowStore) load() (WorkflowState, error) {
	for _, path := range []string{s.Path, s.eventsPath()} {
		info, err := os.Lstat(path)
		if err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0) {
			return WorkflowState{}, fmt.Errorf("workflow files must be regular files, never symlinks")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return WorkflowState{}, fmt.Errorf("cannot inspect workflow files")
		}
	}
	var snapshot WorkflowState
	data, stateErr := os.ReadFile(s.Path)
	if stateErr == nil {
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return WorkflowState{}, fmt.Errorf("task state is corrupt")
		}
		if err := validateWorkflowState(snapshot); err != nil {
			return WorkflowState{}, err
		}
	} else if !errors.Is(stateErr, os.ErrNotExist) {
		return WorkflowState{}, fmt.Errorf("cannot read task state")
	}
	file, err := os.OpenFile(s.eventsPath(), os.O_RDWR, 0600)
	if err != nil {
		return WorkflowState{}, fmt.Errorf("task journal is unavailable")
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var latest WorkflowState
	var offset int64
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(line) > 0 {
				// An interrupted append has no durable newline; preserve prior events.
				if err := file.Truncate(offset); err != nil {
					return WorkflowState{}, fmt.Errorf("cannot recover interrupted journal append")
				}
				if err := file.Sync(); err != nil {
					return WorkflowState{}, err
				}
			}
			break
		}
		if err != nil {
			return WorkflowState{}, fmt.Errorf("cannot read task journal")
		}
		var event workflowEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return WorkflowState{}, fmt.Errorf("task journal is corrupt")
		}
		hash := event.Hash
		event.Hash = ""
		encoded, _ := json.Marshal(event)
		if hash != bytesSHA256(encoded) || event.PreviousHash != latest.LastEventSHA256 || event.State.Revision != latest.Revision+1 || event.From != latest.Status || event.To != event.State.Status || event.State.LastEventSHA256 != "" {
			return WorkflowState{}, fmt.Errorf("task journal chain or revision is invalid")
		}
		if err := validateWorkflowState(event.State); err != nil {
			return WorkflowState{}, err
		}
		if err := validateWorkflowAudit(event.Audit); err != nil {
			return WorkflowState{}, err
		}
		if latest.Revision > 0 && (latest.TaskID != event.State.TaskID || latest.Workspace != event.State.Workspace || (latest.Status != event.State.Status && !allowedTransition(latest.Status, event.State.Status))) {
			return WorkflowState{}, fmt.Errorf("task journal identity or transition is invalid")
		}
		latest = event.State
		latest.LastEventSHA256 = hash
		offset += int64(len(line))
	}
	if latest.Revision == 0 {
		return WorkflowState{}, fmt.Errorf("task journal contains no committed state")
	}
	if stateErr != nil || snapshot.Revision < latest.Revision {
		if err := s.saveSnapshot(latest); err != nil {
			return WorkflowState{}, fmt.Errorf("cannot recover task snapshot")
		}
	} else {
		snapshotJSON, _ := json.Marshal(snapshot)
		latestJSON, _ := json.Marshal(latest)
		if string(snapshotJSON) != string(latestJSON) {
			return WorkflowState{}, fmt.Errorf("task snapshot differs from committed journal")
		}
	}
	return latest, nil
}

func writeWorkflowJournal(file *os.File, data []byte) error {
	count, err := file.Write(data)
	if err != nil {
		return err
	}
	if count != len(data) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func (s *WorkflowStore) saveSnapshot(state WorkflowState) error {
	writer := s.snapshotWriter
	if writer == nil {
		writer = atomicWorkflowJSON
	}
	err := writer(s.Path, state)
	if err == nil {
		// A committed snapshot makes any previously verified copy stale.
		invalidateWorkflowReadCache(s.Path)
	}
	return err
}

func (s *WorkflowStore) eventsPath() string {
	return filepath.Join(filepath.Dir(s.Path), "events.jsonl")
}

func (s *WorkflowStore) locked(ctx context.Context, fn func() error) error {
	if s.Path == "" {
		return fmt.Errorf("task state path is required")
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return fmt.Errorf("cannot create task directory")
	}
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return fmt.Errorf("cannot open task lock")
	}
	defer lock.Close()
	if err := lock.Chmod(0600); err != nil {
		return fmt.Errorf("cannot protect task lock")
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("cannot acquire task lock")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func atomicWorkflowJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".task-state-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func bytesSHA256(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
