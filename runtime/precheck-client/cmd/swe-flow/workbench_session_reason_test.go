package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSessionCaptureReasonUsesToolFixedMessage(t *testing.T) {
	if got := sessionCaptureReason("noise\nsession_capture=BLOCKED 原串会话与指定工作区不一致\n"); got != "原串会话与指定工作区不一致" {
		t.Fatalf("reason=%q", got)
	}
	if got := sessionCaptureReason(""); got != "Session 与目标工作区或真实日志不匹配" {
		t.Fatalf("fallback=%q", got)
	}
	if !transientSessionCapture("核验期间日志发生变化，请重新核验") {
		t.Fatal("log churn must be retryable")
	}
	if transientSessionCapture("原串会话与指定工作区不一致") {
		t.Fatal("identity mismatch must not be retried")
	}
}

func TestRunSessionCaptureReturnsToolReason(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "capture.py")
	if err := os.WriteFile(tool, []byte("import sys\nprint('session_capture=BLOCKED 历史会话与指定工作区不一致')\nsys.exit(1)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reason, err := runSessionCapture(context.Background(), tool, "plan", "workspace", "state", "token", false)
	if err == nil || reason != "历史会话与指定工作区不一致" {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
}
