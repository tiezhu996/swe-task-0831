package main

import (
	"context"
	client "example.com/swe-precheck-client"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const precheckCacheInterval = 5 * time.Minute
const precheckRetryInterval = time.Minute

type cachedPrecheckBinding struct {
	RepoURL      string `json:"repo_url"`
	PromptSHA256 string `json:"prompt_sha256"`
}
type persistedPrecheckCache struct {
	SchemaVersion int                           `json:"schema_version"`
	Root          string                        `json:"root"`
	Account       string                        `json:"account,omitempty"`
	SyncedAt      time.Time                     `json:"synced_at"`
	Items         []client.BatchItem            `json:"items"`
	Bindings      map[int]cachedPrecheckBinding `json:"bindings"`
}

func (w *workbenchServer) loadPrecheckCache() {
	path := filepath.Join(w.privateRoot, "prechecks-cache.json")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16<<20 {
		return
	}
	var snapshot persistedPrecheckCache
	if client.ReadLocalJSON(path, &snapshot) != nil || snapshot.SchemaVersion != 1 || snapshot.Root != w.root || snapshot.SyncedAt.IsZero() || snapshot.SyncedAt.After(time.Now().Add(time.Minute)) || snapshot.Account != activeSWEUsername() {
		return
	}
	w.prechecks = snapshot.Items
	w.prechecksAt = snapshot.SyncedAt
	w.precheckHashes = snapshot.Bindings
}
func (w *workbenchServer) syncPrecheckCache(ctx context.Context) error {
	w.precheckMu.Lock()
	defer w.precheckMu.Unlock()
	return w.syncPrecheckCacheLocked(ctx)
}

func (w *workbenchServer) ensurePrecheckCache(ctx context.Context, force bool) error {
	w.precheckMu.Lock()
	defer w.precheckMu.Unlock()
	w.mu.Lock()
	ready := !w.prechecksAt.IsZero()
	w.mu.Unlock()
	if ready && !force {
		return nil
	}
	return w.syncPrecheckCacheLocked(ctx)
}
func (w *workbenchServer) syncPrecheckCacheLocked(ctx context.Context) (syncErr error) {
	w.mu.Lock()
	w.prechecksRefreshing = true
	w.prechecksAttemptAt = time.Now()
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.prechecksRefreshing = false; w.prechecksError = syncErr != nil; w.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	platform, err := w.newClient(ctx)
	if err != nil {
		return fmt.Errorf("平台登录不可用")
	}
	items, err := platform.ListAllItems(ctx, client.ItemQuery{PageSize: 100})
	if err != nil {
		return fmt.Errorf("个人预检读取失败")
	}
	paths, err := w.taskPaths()
	if err != nil {
		return fmt.Errorf("本地任务不可读，保留旧缓存")
	}
	repos := map[string]bool{}
	registry, registryErr := client.DefaultPrecheckDesignStore()
	if registryErr != nil {
		return registryErr
	}
	designRepos, registryErr := registry.Repositories(ctx)
	if registryErr != nil {
		return registryErr
	}
	for repo := range designRepos {
		repos[repo] = true
	}
	for _, path := range paths {
		state, err := client.NewWorkflowStore(path).Load(ctx)
		if err != nil {
			continue
		}
		repos[state.Project.RepoURL] = true
		if state.Submission.ID > 0 && w.runFlow != nil {
			switch state.Status {
			case client.WorkflowSubmitted, client.WorkflowQCPending, client.WorkflowQCRejected:
				_, _ = w.runFlow(ctx, "submission-status", "-state", path)
			}
		}
	}
	hashes := map[int]cachedPrecheckBinding{}
	for _, item := range items {
		if !repos[item.RepoURL] {
			continue
		}
		detail, err := platform.ItemDetail(ctx, item.ID)
		if err != nil || detail.ID != item.ID || detail.RepoURL != item.RepoURL || detail.RequirementPrompt == "" {
			continue
		}
		hashes[item.ID] = cachedPrecheckBinding{RepoURL: detail.RepoURL, PromptSHA256: client.TextSHA256(detail.RequirementPrompt)}
		if detail.Status == "PRECHECK_PASSED" || detail.Status == "PRECHECK_CONSUMED" {
			if _, err := registry.Bind(ctx, detail); err != nil {
				return fmt.Errorf("预检出题设计绑定失败，保留已有缓存")
			}
		}
	}
	snapshot := persistedPrecheckCache{SchemaVersion: 1, Root: w.root, Account: activeSWEUsername(), SyncedAt: time.Now(), Items: items, Bindings: hashes}
	// A failed save keeps the last durable snapshot and reports the failed sync.
	if err := client.SaveLocalJSON(filepath.Join(w.privateRoot, "prechecks-cache.json"), snapshot); err != nil {
		return fmt.Errorf("个人预检缓存保存失败")
	}
	w.mu.Lock()
	w.prechecks = snapshot.Items
	w.prechecksAt = snapshot.SyncedAt
	w.precheckHashes = snapshot.Bindings
	w.mu.Unlock()
	return nil
}
func (w *workbenchServer) refreshPrecheckCacheIfDue() {
	w.mu.Lock()
	due := w.prechecksAt.IsZero() || time.Since(w.prechecksAt) >= precheckCacheInterval
	retryAllowed := w.prechecksAttemptAt.IsZero() || time.Since(w.prechecksAttemptAt) >= precheckRetryInterval
	w.mu.Unlock()
	if !due || !retryAllowed || !w.precheckMu.TryLock() {
		return
	}
	go func() { defer w.precheckMu.Unlock(); _ = w.syncPrecheckCacheLocked(context.Background()) }()
}
func (w *workbenchServer) startPrecheckCache(ctx context.Context) {
	go func() {
		// Warm before the user opens the tab. Reads of a saved cache never wait here.
		if w.precheckMu.TryLock() {
			_ = w.syncPrecheckCacheLocked(ctx)
			w.precheckMu.Unlock()
		}
		ticker := time.NewTicker(precheckCacheInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !w.precheckMu.TryLock() {
					continue
				}
				_ = w.syncPrecheckCacheLocked(ctx)
				w.precheckMu.Unlock()
			}
		}
	}()
}
