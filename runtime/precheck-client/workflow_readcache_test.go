package precheckclient

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func advanceForCacheTest(t *testing.T, store *WorkflowStore, state WorkflowState, next WorkflowStatus) WorkflowState {
	t.Helper()
	advanced, err := store.Advance(context.Background(), state.Revision, next, WorkflowAudit{Action: "advance", Outcome: "success"})
	if err != nil {
		t.Fatalf("advance to %s failed: %v", next, err)
	}
	return advanced
}

func TestWorkflowLoadServesUnchangedJournalFromCache(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-cache", dir)
	if err != nil {
		t.Fatal(err)
	}
	state = advanceForCacheTest(t, store, state, WorkflowProjectSelected)

	first, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loadVerifiedFromCache(store.Path); !ok {
		t.Fatal("a verified load was not cached")
	}
	second, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("cached state differs from the verified state:\nfirst=%+v\nsecond=%+v", first, second)
	}
	if second.Revision != state.Revision || second.LastEventSHA256 != state.LastEventSHA256 {
		t.Fatalf("cached state is not the committed state: %+v", second)
	}
}

func TestWorkflowLoadCacheFollowsNewCommit(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-cache-commit", dir)
	if err != nil {
		t.Fatal(err)
	}
	state = advanceForCacheTest(t, store, state, WorkflowProjectSelected)
	if _, err := store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadVerifiedFromCache(store.Path); !ok {
		t.Fatal("expected a cached entry before the commit")
	}

	state = advanceForCacheTest(t, store, state, WorkflowPrecheckDraft)
	if _, ok := loadVerifiedFromCache(store.Path); ok {
		t.Fatal("a committed snapshot left a stale cache entry behind")
	}
	loaded, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != state.Revision {
		t.Fatalf("stale revision served after commit: got %d want %d", loaded.Revision, state.Revision)
	}
}

func TestWorkflowLoadCacheRejectsTamperedJournal(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-cache-tamper", dir)
	if err != nil {
		t.Fatal(err)
	}
	state = advanceForCacheTest(t, store, state, WorkflowProjectSelected)
	if _, err := store.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadVerifiedFromCache(store.Path); !ok {
		t.Fatal("expected a cached entry before tampering")
	}

	journal := filepath.Join(dir, "events.jsonl")
	file, err := os.OpenFile(journal, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Load(ctx); err == nil {
		t.Fatal("a tampered journal was served from cache instead of being rejected")
	}
	if _, ok := loadVerifiedFromCache(store.Path); ok {
		t.Fatal("a rejected journal left a cache entry behind")
	}
}

func TestWorkflowLoadCacheReturnsIndependentCopies(t *testing.T) {
	store, dir := newWorkflowTestStore(t)
	ctx := context.Background()
	state, err := store.Init(ctx, "task-cache-copy", dir)
	if err != nil {
		t.Fatal(err)
	}
	state = advanceForCacheTest(t, store, state, WorkflowProjectSelected)
	if _, err := store.Load(ctx); err != nil {
		t.Fatal(err)
	}

	first, ok := loadVerifiedFromCache(store.Path)
	if !ok {
		t.Fatal("expected a cache hit")
	}
	second, ok := loadVerifiedFromCache(store.Path)
	if !ok {
		t.Fatal("expected a second cache hit")
	}
	first.Verification.Artifacts = map[string]WorkflowArtifact{"probe": {}}
	first.TaskID = "mutated"
	if _, found := second.Verification.Artifacts["probe"]; found {
		t.Fatal("cache hits share a mutable artifacts map")
	}
	if second.TaskID == "mutated" {
		t.Fatal("cache hits share mutable state")
	}
	if second.Revision != state.Revision {
		t.Fatalf("unexpected cached revision %d", second.Revision)
	}
}
