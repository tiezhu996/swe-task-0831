package main

import (
	"context"
	client "example.com/swe-precheck-client"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func traceDropDirectory(path string) string { return filepath.Join(filepath.Dir(path), "trajectories") }
func ensureTraceDirectory(path string) error {
	dir := traceDropDirectory(path)
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return fmt.Errorf("无法创建本题轨迹目录")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("轨迹目录不能是符号链接")
	}
	return nil
}
func traceArchiveDirectory(state client.WorkflowState, taskPaths ...string) string {
	if a, ok := state.Verification.Artifacts["x_trae_trace"]; ok {
		return filepath.Dir(a.Path)
	}
	root := filepath.Dir(state.PlanPath)
	if state.PlanPath == "" && len(taskPaths) > 0 {
		root = filepath.Dir(taskPaths[0])
	}
	if _, err := os.Stat(filepath.Join(root, "trae-export-sanitized.receipt.json")); err == nil {
		return root
	}
	return filepath.Join(root, "trajectories", "archive")
}

type traceFileView struct {
	Name       string `json:"name"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	ModifiedAt string `json:"modified_at"`
	Problem    string `json:"problem,omitempty"`
}

func (w *workbenchServer) traceFiles(out http.ResponseWriter, r *http.Request) {
	path, _, err := w.loadTask(r.Context(), r.PathValue("key"))
	if err != nil {
		workbenchError(out, 409, "任务不存在或审计链不可用")
		return
	}
	if err := ensureTraceDirectory(path); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	entries, err := os.ReadDir(traceDropDirectory(path))
	if err != nil {
		workbenchError(out, 409, "轨迹目录不可读")
		return
	}
	files := []traceFileView{}
	for _, entry := range entries {
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		item := traceFileView{Name: entry.Name(), Bytes: info.Size(), ModifiedAt: info.ModTime().UTC().Format(time.RFC3339)}
		switch {
		case entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular():
			item.Problem = "仅支持普通 Markdown 文件，不读取符号链接"
		case info.Size() == 0:
			item.Problem = "文件为空"
		case info.Size() > 32<<20:
			item.Problem = "文件超过 32 MiB"
		default:
			artifact, err := client.HashWorkflowArtifact(filepath.Join(traceDropDirectory(path), entry.Name()))
			if err != nil {
				item.Problem = "文件不可读"
			} else {
				item.SHA256 = artifact.SHA256
			}
		}
		files = append(files, item)
		if len(files) > 100 {
			workbenchError(out, 409, "Markdown 超过 100 份，请整理本题目录后检测")
			return
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	writeWorkbenchJSON(out, 200, map[string]any{"directory": traceDropDirectory(path), "files": files})
}
func (w *workbenchServer) importTraceFile(out http.ResponseWriter, r *http.Request) {
	w.evidenceMu.Lock()
	defer w.evidenceMu.Unlock()
	path, state, ok := w.actionTask(out, r)
	if !ok {
		return
	}
	if state.Status != client.WorkflowSessionVerified {
		workbenchError(out, 409, "先核验本题 Session，再绑定目录轨迹")
		return
	}
	var input struct {
		Name     string `json:"name"`
		SHA256   string `json:"sha256"`
		Attested bool   `json:"attested"`
	}
	if decodeWorkbenchJSON(r, &input) != nil || !input.Attested || input.Name == "" || filepath.Base(input.Name) != input.Name || strings.ContainsAny(input.Name, "/\\") || !strings.EqualFold(filepath.Ext(input.Name), ".md") || input.SHA256 == "" {
		workbenchError(out, 400, "请选择检测到的本题原生 Markdown")
		return
	}
	if err := ensureTraceDirectory(path); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	source := filepath.Join(traceDropDirectory(path), input.Name)
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 32<<20 {
		workbenchError(out, 409, "文件不是可读取的普通 Markdown")
		return
	}
	artifact, err := client.HashWorkflowArtifact(source)
	if err != nil || artifact.SHA256 != input.SHA256 {
		workbenchError(out, 409, "文件在检测后发生变化，请重新检测")
		return
	}
	if err := w.archiveTraceExpected(r.Context(), path, state, source, input.SHA256); err != nil {
		workbenchError(out, 409, err.Error())
		return
	}
	writeWorkbenchJSON(out, 200, w.taskView(r.Context(), path))
}
func (w *workbenchServer) archiveTrace(ctx context.Context, path string, state client.WorkflowState, source string) error {
	return w.archiveTraceExpected(ctx, path, state, source, "")
}
func (w *workbenchServer) archiveTraceExpected(ctx context.Context, path string, state client.WorkflowState, source, expectedHash string) error {
	if state.PlanPath == "" || state.Status != client.WorkflowSessionVerified {
		return fmt.Errorf("先完成 Session 核验与计划绑定")
	}
	if err := ensureTraceDirectory(path); err != nil {
		return err
	}
	for _, candidate := range []string{filepath.Join(filepath.Dir(path), "trae-export-sanitized.receipt.json"), filepath.Join(traceDropDirectory(path), "archive")} {
		if _, err := os.Lstat(candidate); !os.IsNotExist(err) {
			return fmt.Errorf("已有轨迹归档，不能覆盖；请核查已绑定材料")
		}
	}
	scratch, err := os.MkdirTemp(traceDropDirectory(path), ".archive-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)
	input := filepath.Join(scratch, "trae-app-export.md")
	// Copy first, and verify these exact bytes; originals in the drop folder remain.
	fd, err := syscall.Open(source, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	origin := os.NewFile(uintptr(fd), source)
	defer origin.Close()
	info, err := origin.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 32<<20 {
		return fmt.Errorf("轨迹文件大小或类型无效")
	}
	dest, err := os.OpenFile(input, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(dest, io.LimitReader(origin, 32<<20+1))
	closeErr := dest.Close()
	if copyErr != nil || closeErr != nil || n != info.Size() || n > 32<<20 {
		return fmt.Errorf("轨迹读取不完整或正在变化")
	}
	if expectedHash != "" {
		artifact, err := client.HashWorkflowArtifact(input)
		if err != nil || artifact.SHA256 != expectedHash {
			return fmt.Errorf("文件在检测后发生变化，请重新检测")
		}
	}
	tool, err := workbenchTool(w.root, "collect_trae_export.py")
	if err != nil {
		return err
	}
	cleaned := filepath.Join(scratch, "trae-export-sanitized.md")
	cmd := exec.CommandContext(ctx, "python3", tool, "--input", input, "--plan", state.PlanPath, "--out", cleaned)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if cmd.Run() != nil {
		return fmt.Errorf("轨迹人工消息与冻结题面不一致，或原生导出格式无法核查")
	}
	var receipt struct {
		RawSHA256     string `json:"raw_sha256"`
		PromptSHA256  string `json:"prompt_sha256"`
		SessionSHA256 string `json:"session_sha256"`
	}
	if client.ReadLocalJSON(filepath.Join(scratch, "trae-export-sanitized.receipt.json"), &receipt) != nil {
		return fmt.Errorf("轨迹回执不可读")
	}
	attestation := traceAttestation{Source: "human_confirmed_native_export", RawSHA256: receipt.RawSHA256, PromptSHA256: receipt.PromptSHA256, SessionSHA256: receipt.SessionSHA256, ConfirmedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := client.SaveLocalJSON(filepath.Join(scratch, "trae-source-attestation.json"), attestation); err != nil {
		return err
	}
	archive := filepath.Join(traceDropDirectory(path), "archive")
	if err := os.Rename(scratch, archive); err != nil {
		return fmt.Errorf("归档失败，原始文件保留")
	}
	plan, err := client.ReadSubmissionPlan(state.PlanPath)
	if err != nil {
		return err
	}
	if plan.Attachments == nil {
		plan.Attachments = map[string]string{}
	}
	plan.Attachments["x_trae_trace"] = filepath.Join(archive, "trae-export-sanitized.md")
	if err := client.SaveLocalJSON(state.PlanPath, plan); err != nil {
		return err
	}
	if _, err := w.runFlow(ctx, "bind-plan", "-state", path, "-plan", state.PlanPath); err != nil {
		return fmt.Errorf("轨迹已归档，但状态绑定失败，需核查已有归档")
	}
	return nil
}
