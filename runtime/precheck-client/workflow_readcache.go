package precheckclient

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Read-side cache for a verified journal replay.
//
// Load replays a task's complete hash chain: every event line is unmarshalled,
// re-marshalled, SHA-256'd, then validated. The workbench lists every task on
// each refresh, so unchanged journals were being re-verified several times a
// minute. This cache returns the already verified state for as long as both
// files that make up the journal are demonstrably unchanged.
//
// The cache never authorises a write. Update, Advance and commit all call
// load() directly and re-verify the chain, and every successful snapshot write
// drops the entry. A hit also hands back a fresh copy decoded from cached
// bytes, so no caller can share mutable state with the cache.
const (
	// Entries are only ever addressed by exact file identity, so the limit
	// exists to bound memory, not to shorten a hit.
	workflowReadCacheLimit = 256
	// A generous backstop in case a filesystem reports a stamp that does not
	// move even though bytes changed.
	workflowReadCacheTTL = 30 * time.Minute
)

// workflowFileStamp identifies one observed version of a file. Size and
// nanosecond modification time both have to match for a cached state to be
// reused; any append, rewrite or truncation changes at least one of them.
type workflowFileStamp struct {
	Size int64
	Mod  int64
}

// workflowJournalStamp is the pair of files a verified state depends on.
type workflowJournalStamp struct {
	State   workflowFileStamp
	Journal workflowFileStamp
}

type cachedWorkflowState struct {
	Stamp   workflowJournalStamp
	Payload []byte
	Stored  time.Time
}

type workflowReadCacheStore struct {
	sync.Mutex
	entries map[string]cachedWorkflowState
}

var workflowReadCache = &workflowReadCacheStore{entries: make(map[string]cachedWorkflowState)}

// workflowFileStampOf mirrors the regular-file, no-symlink rule that load()
// enforces before reading either workflow file.
func workflowFileStampOf(path string) (workflowFileStamp, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return workflowFileStamp{}, false
	}
	return workflowFileStamp{Size: info.Size(), Mod: info.ModTime().UnixNano()}, true
}

// workflowStoreStamp stamps the state snapshot and the journal together.
func workflowStoreStamp(statePath string) (workflowJournalStamp, bool) {
	if statePath == "" {
		return workflowJournalStamp{}, false
	}
	state, ok := workflowFileStampOf(statePath)
	if !ok {
		return workflowJournalStamp{}, false
	}
	journal, ok := workflowFileStampOf(filepath.Join(filepath.Dir(statePath), "events.jsonl"))
	if !ok {
		return workflowJournalStamp{}, false
	}
	return workflowJournalStamp{State: state, Journal: journal}, true
}

// loadVerifiedFromCache returns an independent copy of a state that was
// verified while both files carried exactly the stamps recorded here.
func loadVerifiedFromCache(statePath string) (WorkflowState, bool) {
	current, ok := workflowStoreStamp(statePath)
	if !ok {
		return WorkflowState{}, false
	}
	workflowReadCache.Lock()
	entry, hit := workflowReadCache.entries[statePath]
	workflowReadCache.Unlock()
	if !hit || entry.Stamp != current || time.Since(entry.Stored) > workflowReadCacheTTL {
		return WorkflowState{}, false
	}
	var state WorkflowState
	if err := json.Unmarshal(entry.Payload, &state); err != nil {
		return WorkflowState{}, false
	}
	return state, true
}

// storeVerifiedInCache records a state that load() verified. The stamp must
// have been read while the caller held the task lock, so a journal that moved
// after verification cannot be cached under the newer stamp.
func storeVerifiedInCache(statePath string, stamp workflowJournalStamp, state WorkflowState) {
	if statePath == "" {
		return
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return
	}
	entry := cachedWorkflowState{Stamp: stamp, Payload: payload, Stored: time.Now()}
	workflowReadCache.Lock()
	defer workflowReadCache.Unlock()
	if len(workflowReadCache.entries) >= workflowReadCacheLimit {
		workflowReadCache.evictLocked()
	}
	workflowReadCache.entries[statePath] = entry
}

// evictLocked drops expired entries, then clears the map if the bound is still
// reached. Clearing costs one re-verification per live task; that is cheaper
// than tracking recency for a cache whose hits are already bounded by file
// identity.
func (c *workflowReadCacheStore) evictLocked() {
	now := time.Now()
	for path, entry := range c.entries {
		if now.Sub(entry.Stored) > workflowReadCacheTTL {
			delete(c.entries, path)
		}
	}
	if len(c.entries) >= workflowReadCacheLimit {
		c.entries = make(map[string]cachedWorkflowState)
	}
}

// invalidateWorkflowReadCache drops one task's verified state. Every write path
// calls this, so a committed event is never served from a stale entry.
func invalidateWorkflowReadCache(statePath string) {
	if statePath == "" {
		return
	}
	workflowReadCache.Lock()
	delete(workflowReadCache.entries, statePath)
	workflowReadCache.Unlock()
}
