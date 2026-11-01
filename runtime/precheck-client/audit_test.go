package precheckclient

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func auditedFixture(t *testing.T) (SubmissionPlan, FormSchema, AuditEvidence) {
	p, s := submissionFixture(t)
	workspace := t.TempDir()
	shot := filepath.Join(workspace, "shot.png")
	var b bytes.Buffer
	_ = png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 10, 10)))
	if err := os.WriteFile(shot, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	p.Attachments["x_artifact_shot"] = shot
	s.Fields = append(s.Fields, FormField{Key: "x_artifact_shot", Type: "attachment", Required: true})
	s.Attachment.Accept = ".md,.png"
	trace := []byte("# Native app export\nactual prompt\nAgent response\n")
	_ = os.WriteFile(p.Attachments["x_trae_trace"], trace, 0600)
	_ = os.WriteFile(filepath.Join(workspace, "verification.log"), []byte("actual verification failure"), 0600)
	_ = os.WriteFile(filepath.Join(workspace, "verification-command.sh"), []byte("exit 1"), 0600)
	exitCode := 1
	e := AuditEvidence{Workspace: workspace,
		Session:           SessionEvidence{TokenSHA256: TextSHA256(syntheticToken()), WorkspaceVerified: true, WorkspaceSHA256: TextSHA256(workspace), EffectiveRoundCount: 120, MatchingLogCount: 1, Applied: true},
		Terminal:          NativeTerminalEvidence{Source: "native_terminal", Completed: true, ExecutionCompleted: true, ExitCode: &exitCode, CapturedAt: "2026-10-05T16:17:50Z", PrivacyCrop: &NativePrivacyCrop{Reason: "native window title", PixelBox: []int{0, 34, 800, 600}}, ScreenshotSHA256: TextSHA256(b.String()), LogSHA256: TextSHA256("actual verification failure"), CommandSHA256: TextSHA256("exit 1"), LogPath: "verification.log", CommandPath: "verification-command.sh"},
		Trace:             TraceEvidence{Source: "trae_cn_app_export", SHA256: TextSHA256(string(trace)), PromptSHA256: TextSHA256(p.ExecutionPrompt), SessionSHA256: TextSHA256(syntheticToken())},
		TerminalDirectory: workspace,
	}
	return p, s, e
}

func TestEvidenceAuditDoesNotConfuseFailedTaskWithMissingEvidence(t *testing.T) {
	p, s, e := auditedFixture(t)
	r := AuditSubmission(p, s, e)
	if !r.Passed || r.NetworkWrites != 0 || r.RemarksSent {
		t.Fatalf("%v", AuditFailures(r))
	}
}

func TestEvidenceAuditAllowsLocalReviewWhileSubmissionIsLimited(t *testing.T) {
	p, s, e := auditedFixture(t)
	s.Limited = true
	r := AuditSubmission(p, s, e)
	if !r.Passed || r.NetworkWrites != 0 {
		t.Fatalf("limited submission should still allow local audit: %v", AuditFailures(r))
	}
}

func TestNativeDisplayV2HashBinding(t *testing.T) {
	p, s, e := auditedFixture(t)
	display := "Rubric result\n1 FAIL actual failure\nexit_code=1\n"
	path := filepath.Join(e.TerminalDirectory, "terminal-display.log")
	_ = os.WriteFile(path, []byte(display), 0600)
	e.Terminal.SchemaVersion = 2
	e.Terminal.DisplayMode = "rubric_results"
	e.Terminal.DisplayPath = "terminal-display.log"
	e.Terminal.DisplaySHA256 = TextSHA256(display)
	if !AuditSubmission(p, s, e).Passed {
		t.Fatal("bound actual failed result should be accepted")
	}
	_ = os.WriteFile(path, []byte("1 PASS changed"), 0600)
	if AuditSubmission(p, s, e).Passed {
		t.Fatal("changed display accepted")
	}
}

func TestAuditBlocksStaleAndSimulatedEvidence(t *testing.T) {
	for _, scenario := range []string{"rounds", "workspace", "token", "html", "attachment_changed", "trace_source", "existing", "execution_incomplete", "capture_error", "exit_code_missing", "privacy_crop_missing", "content_modified", "capture_time_missing", "absolute_log"} {
		t.Run(scenario, func(t *testing.T) {
			p, s, e := auditedFixture(t)
			switch scenario {
			case "rounds":
				e.Session.EffectiveRoundCount++
			case "workspace":
				e.Workspace = "/another-workspace"
			case "token":
				e.Session.TokenSHA256 = strings.Repeat("0", 64)
			case "html":
				e.Terminal.Source = "browser_html"
			case "attachment_changed":
				_ = os.WriteFile(p.Attachments["x_trae_trace"], []byte("actual prompt modified"), 0600)
			case "trace_source":
				e.Trace.Source = "reconstructed"
			case "existing":
				p.ExistingSubmissionID = 1481
			case "execution_incomplete":
				e.Terminal.ExecutionCompleted = false
			case "capture_error":
				e.Terminal.ErrorCode = "verification_timeout"
			case "exit_code_missing":
				e.Terminal.ExitCode = nil
			case "privacy_crop_missing":
				e.Terminal.PrivacyCrop = nil
			case "content_modified":
				e.Terminal.PrivacyCrop.ContentPixelsModified = true
			case "capture_time_missing":
				e.Terminal.CapturedAt = ""
			case "absolute_log":
				e.Terminal.LogPath = filepath.Join(e.TerminalDirectory, "verification.log")
			}
			r := AuditSubmission(p, s, e)
			if r.Passed || r.NetworkWrites != 0 {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}

func TestNativeEvidenceExitCodeMustBePresent(t *testing.T) {
	var missing NativeTerminalEvidence
	if err := json.Unmarshal([]byte(`{"completed":true,"execution_completed":true}`), &missing); err != nil {
		t.Fatal(err)
	}
	if missing.ExitCode != nil {
		t.Fatal("missing exit code became a successful zero exit")
	}
	var success NativeTerminalEvidence
	if err := json.Unmarshal([]byte(`{"exit_code":0}`), &success); err != nil {
		t.Fatal(err)
	}
	if success.ExitCode == nil || *success.ExitCode != 0 {
		t.Fatal("actual successful zero exit was lost")
	}
}

func TestEvidenceAuditAcceptsAHistoricalSnapshotReceipt(t *testing.T) {
	p, s, e := auditedFixture(t)
	// A historical receipt is proven from the per-session snapshot repository,
	// so it has no matched renderer log and must still pass the local audit.
	e.Session.Source = "historical_session_snapshot"
	e.Session.SessionSource = "trae_session_snapshot"
	e.Session.MatchingLogCount = 0
	if r := AuditSubmission(p, s, e); !r.Passed {
		t.Fatalf("historical receipt rejected: %v", AuditFailures(r))
	}
}

func TestEvidenceAuditStillRequiresAMatchedLogForALogBackedReceipt(t *testing.T) {
	p, s, e := auditedFixture(t)
	e.Session.MatchingLogCount = 0
	if r := AuditSubmission(p, s, e); r.Passed {
		t.Fatal("log-backed receipt without a matched log passed")
	}
}
