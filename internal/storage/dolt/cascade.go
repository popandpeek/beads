// POPANDPEEK-FORK BEGIN: cascade bd update / bd close from parent to children (be-merbj.3)
// Cascade propagates select fields — status, closed_at, external_ref, close_reason —
// from a parent bead to all of its parent-child descendants in a single Pattern A
// runDoltTransaction. This keeps the parent and all children consistent with one
// another and atomic on a single Dolt commit, so crew / polecat status tracking
// never ends up half-applied. Per be-nxe8m tech debt we use Pattern A (the pinned
// runDoltTransaction flow in transaction.go) to avoid the Pattern B pitfalls that
// barry hit earlier (silent sql.Tx loss when DOLT_COMMIT reports nothing to stage).
package dolt

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/steveyegge/beads/internal/storage/issueops"
	"github.com/steveyegge/beads/internal/types"
)

// cascadableUpdateFields is the closed set of update fields that propagate from a
// parent bead to its parent-child descendants. All other fields (title, assignee,
// priority, description, etc.) stay local to the bead they were set on.
var cascadableUpdateFields = []string{
	"status",
	"closed_at",
	"external_ref",
	"close_reason",
}

// findParentChildDescendantsInTx returns every transitive parent-child descendant
// of rootID, in BFS order, excluding rootID itself. It uses the shared
// issueops.GetChildrenOfIssuesInTx helper which already queries both the
// permanent dependencies table and the wisp_dependencies table, so mixed
// issue/wisp hierarchies are handled transparently.
//
// A visited set guards against malformed graphs with pre-existing cycles so this
// helper cannot spin forever even if the on-disk state is corrupt — see
// DetectParentCycleInTx for the prevention side of the same invariant.
func findParentChildDescendantsInTx(ctx context.Context, tx *sql.Tx, rootID string) ([]string, error) {
	visited := map[string]bool{rootID: true}
	var result []string
	frontier := []string{rootID}

	for len(frontier) > 0 {
		children, err := issueops.GetChildrenOfIssuesInTx(ctx, tx, frontier)
		if err != nil {
			return nil, fmt.Errorf("find descendants of %s: %w", rootID, err)
		}
		var nextFrontier []string
		for _, childID := range children {
			if visited[childID] {
				continue
			}
			visited[childID] = true
			result = append(result, childID)
			nextFrontier = append(nextFrontier, childID)
		}
		frontier = nextFrontier
	}
	return result, nil
}

// cascadeCloseReason builds the close_reason written to each cascaded child.
// Per acceptance criteria, every cascaded close carries a "via parent <id>"
// suffix so the audit trail records how the close propagated. When the parent
// close had no reason, the suffix is the whole reason.
func cascadeCloseReason(parentReason, parentID string) string {
	suffix := "via parent " + parentID
	if parentReason == "" {
		return suffix
	}
	return parentReason + " " + suffix
}

// isClosingUpdate reports whether updates sets status to StatusClosed.
// Used to decide whether to apply the "via parent" close_reason suffix when
// cascading through UpdateIssue (as opposed to a direct CloseIssue call).
func isClosingUpdate(updates map[string]interface{}) bool {
	raw, ok := updates["status"]
	if !ok {
		return false
	}
	switch v := raw.(type) {
	case string:
		return v == string(types.StatusClosed)
	case types.Status:
		return v == types.StatusClosed
	}
	return false
}

// markCascadeDirty marks every table that a cascade may have written to.
// Cascaded children may live in either issues or wisps (mixed hierarchies are
// legal — see issueops.GetChildrenOfIssuesInTx). Wisp tables are dolt-ignored,
// so marking them here is cheap and harmless if no wisp children exist.
func markCascadeDirty(dt *doltTransaction) {
	dt.dirty.MarkDirty("issues")
	dt.dirty.MarkDirty("events")
	dt.dirty.MarkDirty("wisps")
	dt.dirty.MarkDirty("wisp_events")
}

// cascadeCloseToDescendants closes every open parent-child descendant of parentID
// inside the given transaction. Each descendant's close_reason is suffixed with
// "via parent <parentID>". Already-closed descendants are skipped so the
// operation is idempotent across retries.
func cascadeCloseToDescendants(ctx context.Context, dt *doltTransaction, parentID, parentReason, actor, session string) error {
	descendants, err := findParentChildDescendantsInTx(ctx, dt.tx, parentID)
	if err != nil {
		return err
	}
	if len(descendants) == 0 {
		return nil
	}

	for _, childID := range descendants {
		child, err := issueops.GetIssueInTx(ctx, dt.tx, childID)
		if err != nil {
			return fmt.Errorf("cascade close: load %s: %w", childID, err)
		}
		if child == nil || child.Status == types.StatusClosed {
			continue
		}
		reason := cascadeCloseReason(parentReason, parentID)
		if _, err := issueops.CloseIssueInTx(ctx, dt.tx, childID, reason, actor, session); err != nil {
			return fmt.Errorf("cascade close: close %s: %w", childID, err)
		}
	}
	markCascadeDirty(dt)
	return nil
}

// cascadeUpdateToDescendants applies the cascadable subset of updates to every
// parent-child descendant of parentID. If updates closes the parent (status →
// closed), the cascaded close_reason on each descendant is suffixed with
// "via parent <parentID>" so the propagation is visible in history.
// Descendants already in the target terminal state (closed) are skipped to
// avoid noisy no-op events and to keep retries idempotent.
func cascadeUpdateToDescendants(ctx context.Context, dt *doltTransaction, parentID string, updates map[string]interface{}, actor string) error {
	cascadeUpdates := extractCascadable(updates)
	if len(cascadeUpdates) == 0 {
		return nil
	}

	closing := isClosingUpdate(cascadeUpdates)
	if closing {
		original, _ := cascadeUpdates["close_reason"].(string)
		cascadeUpdates["close_reason"] = cascadeCloseReason(original, parentID)
	}

	descendants, err := findParentChildDescendantsInTx(ctx, dt.tx, parentID)
	if err != nil {
		return err
	}
	if len(descendants) == 0 {
		return nil
	}

	for _, childID := range descendants {
		child, err := issueops.GetIssueInTx(ctx, dt.tx, childID)
		if err != nil {
			return fmt.Errorf("cascade update: load %s: %w", childID, err)
		}
		if child == nil {
			continue
		}
		if closing && child.Status == types.StatusClosed {
			continue
		}
		perChild := copyUpdates(cascadeUpdates)
		if _, err := issueops.UpdateIssueInTx(ctx, dt.tx, childID, perChild, actor); err != nil {
			return fmt.Errorf("cascade update: apply to %s: %w", childID, err)
		}
	}
	markCascadeDirty(dt)
	return nil
}

// extractCascadable returns a new map containing only the cascadable fields
// present in updates. Returns an empty map (not nil) when nothing cascades,
// so the caller can test with len(...) without nil-checks.
func extractCascadable(updates map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(cascadableUpdateFields))
	for _, field := range cascadableUpdateFields {
		if v, ok := updates[field]; ok {
			out[field] = v
		}
	}
	return out
}

// copyUpdates returns a shallow copy so issueops.UpdateIssueInTx cannot mutate
// the shared cascadeUpdates map across descendants.
func copyUpdates(updates map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		out[k] = v
	}
	return out
}

// POPANDPEEK-FORK END
