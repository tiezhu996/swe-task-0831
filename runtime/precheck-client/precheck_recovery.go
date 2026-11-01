package precheckclient

import (
	"fmt"
	"time"
)

// A matching prompt proves content, not that an ambiguous rerun was queued.
func ValidatePrecheckRecovery(state WorkflowState, item ItemDetail) error {
	op := state.PendingOperation
	if op == nil || (op.Outcome != "started" && op.Outcome != "uncertain") || state.Status != WorkflowPrecheckDraft || !op.PrecheckSnapshotTaken {
		return fmt.Errorf("missing unresolved precheck request or recovery snapshot")
	}
	hash, _ := RowsSHA256([]Row{{RepoURL: item.RepoURL, Prompt: item.RequirementPrompt}})
	if item.ID <= 0 || item.BatchID == "" || item.RepoURL != state.Project.RepoURL || hash != op.PayloadSHA256 {
		return fmt.Errorf("precheck recovery content or identity differs")
	}
	switch op.Kind {
	case "precheck_submit":
		for _, id := range op.PriorPrecheckItemIDs {
			if id == item.ID {
				return fmt.Errorf("existing precheck cannot confirm a new submission")
			}
		}
	case "precheck_repair", "precheck_rerun":
		before := op.PrecheckBefore
		if before == nil || (item.ID == before.ItemID && item.BatchID == before.BatchID) {
			return fmt.Errorf("no new precheck item or batch; write remains uncertain")
		}
	default:
		return fmt.Errorf("not an unresolved precheck operation")
	}
	started, err := time.Parse(time.RFC3339Nano, op.StartedAt)
	if err != nil {
		return fmt.Errorf("invalid recovery timestamp")
	}
	// Same-item repairs may retain their creation time, so use the update time.
	stamp := item.CreatedAt
	if op.PrecheckBefore != nil && item.ID == op.PrecheckBefore.ItemID {
		stamp = item.UpdatedAt
	}
	observed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil || observed.Before(started) {
		return fmt.Errorf("new precheck run time is unavailable or predates the request")
	}
	return nil
}
