// POPANDPEEK-FORK BEGIN: parent-child cycle detection for bd parent set (be-merbj.3)
// Walking the parent chain on insert prevents `bd update --parent` from ever
// creating a loop like A.parent=B, B.parent=A. Cycles in the parent-child graph
// would wedge cascade traversal, blocked-ID computation, and any other code that
// walks children upward, so we reject them with a typed error (ErrParentCycle)
// that CLI callers can detect and surface cleanly. The check runs inside the
// same transaction that will insert the dependency so two concurrent parent-set
// calls cannot both pass the check and race to create a cycle.
package dolt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrParentCycle indicates that a requested parent-child dependency would
// create a cycle in the parent chain. Callers should detect this with
// errors.Is(err, ErrParentCycle) and surface a user-friendly error.
var ErrParentCycle = errors.New("parent cycle")

// DetectParentCycleInTx returns an error wrapping ErrParentCycle if setting
// childID's parent to newParentID would create a cycle. It walks the parent
// chain upward starting from newParentID: if it ever reaches childID (or if
// childID == newParentID, the trivial self-parent case), that means newParentID
// is already a descendant of childID, and promoting it to parent would close a
// loop.
//
// The walk is bounded by a visited set so a pre-existing corrupt cycle cannot
// make this function loop forever — it terminates and returns nil (no NEW
// cycle introduced), leaving the caller to decide how to handle the corruption.
//
// Both permanent (dependencies) and wisp (wisp_dependencies) tables are
// consulted so mixed hierarchies are walked correctly.
func DetectParentCycleInTx(ctx context.Context, tx *sql.Tx, childID, newParentID string) error {
	if childID == "" || newParentID == "" {
		return nil
	}
	if childID == newParentID {
		return fmt.Errorf("%w: %s cannot be its own parent", ErrParentCycle, childID)
	}

	visited := map[string]bool{}
	current := newParentID
	for current != "" {
		if current == childID {
			return fmt.Errorf("%w: setting parent of %s to %s would form loop", ErrParentCycle, childID, newParentID)
		}
		if visited[current] {
			// The graph already contains a cycle upstream of newParentID. We have
			// not proven that the new edge introduces an additional loop through
			// childID, so do not block the write — leave the corruption for a
			// dedicated repair tool to surface.
			return nil
		}
		visited[current] = true

		parentID, err := lookupParentInTx(ctx, tx, current)
		if err != nil {
			return err
		}
		current = parentID
	}
	return nil
}

// lookupParentInTx returns the parent ID of id via parent-child dependency, or
// "" if id has no parent. Queries dependencies first, then wisp_dependencies so
// the walk works for hierarchies that mix permanent and ephemeral beads.
// A missing wisp_dependencies table (pre-migration databases) is treated as
// "no parent there" rather than a hard error.
func lookupParentInTx(ctx context.Context, tx *sql.Tx, id string) (string, error) {
	for _, depTable := range []string{"dependencies", "wisp_dependencies"} {
		var parentID string
		//nolint:gosec // G201: depTable is hardcoded to "dependencies" or "wisp_dependencies"
		query := fmt.Sprintf(
			"SELECT depends_on_id FROM %s WHERE issue_id = ? AND type = 'parent-child' LIMIT 1",
			depTable,
		)
		err := tx.QueryRowContext(ctx, query, id).Scan(&parentID)
		if err == nil {
			return parentID, nil
		}
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if isTableNotExistError(err) {
			continue
		}
		return "", fmt.Errorf("lookup parent of %s in %s: %w", id, depTable, err)
	}
	return "", nil
}

// POPANDPEEK-FORK END
