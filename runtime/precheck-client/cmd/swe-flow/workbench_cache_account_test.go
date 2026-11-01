package main

import (
	"path/filepath"
	"testing"
	"time"

	client "example.com/swe-precheck-client"
)

func TestPrecheckCacheIsScopedToActiveAccount(t *testing.T) {
	t.Setenv("SWE_USERNAME", "fixture")
	t.Setenv("SWE_PASSWORD", "fixture")
	root := t.TempDir()
	w, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.privateRoot, "prechecks-cache.json")
	write := func(account string) {
		snapshot := persistedPrecheckCache{
			SchemaVersion: 1, Root: w.root, Account: account, SyncedAt: time.Now(),
			Items: []client.BatchItem{{ID: 7, RepoURL: "https://github.com/o/r"}},
		}
		if err := client.SaveLocalJSON(path, snapshot); err != nil {
			t.Fatal(err)
		}
	}
	write("another-user")
	restarted, err := newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.prechecksAt.IsZero() {
		t.Fatal("cache from another account was loaded")
	}
	write("fixture")
	restarted, err = newWorkbenchServer(root)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.prechecksAt.IsZero() || len(restarted.prechecks) != 1 {
		t.Fatal("cache for the active account was not loaded")
	}
}
