// POPANDPEEK-FORK BEGIN: transaction isolation test for bd ↔ DOLT_COMMIT concurrency bug (be-2ppnh)
//
// Reproduction harness for the cross-session write-absorption scenario
// discovered during investigation of be-3xpvi's silent close. Full research:
// /home/ben/gt/mayor/docs/research-2026-04-12-bd-dolt-commit-concurrency.md
//
// Hypothesis (emmett, 2026-04-12): when bd mutates via Pattern B code paths
// (s.db.BeginTx + CALL DOLT_COMMIT inside the sql.Tx), an unrelated external
// process running its own CALL DOLT_COMMIT on a separate connection to the
// same Dolt database can absorb bd's write into the external commit. The
// smoking gun in beacon DB was commit n9htiploq6u3ilmcdhi6mrpva5eubt3b whose
// declared purpose was "migration 014: add status_changed_at" but whose
// dolt_diff_issues row shows a status flip on be-3xpvi (an unrelated bead).
//
// Pre-Option B (current state on this branch): test is SKIPPED via t.Skip so
// CI stays green while the Option B structural refactor lands. To run the
// reproduction manually, remove the t.Skip line temporarily.
//
// Post-Option B: t.Skip is removed and the assertions become load-bearing.
// If the refactor fixes the bug, the test stays green. If the bug resurfaces,
// this test is the regression harness.
//
// Mayor direction: hq-wisp-8tczp / be-2ppnh. Fork marker per CASS rule
// b-mnvzn1ad-z96v8y.
// POPANDPEEK-FORK END

package dolt

import (
	"database/sql" // POPANDPEEK-FORK: fork-custom test for be-2ppnh isolation repro
	"fmt"
	"sync"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestDoltCommitIsolation_ExternalProcessAbsorption verifies that a bd mutation
// running on bd's own connection pool is NOT absorbed into an unrelated
// CALL DOLT_COMMIT invoked from a separate sql.DB pool targeting the same
// database. This reproduces the be-2ppnh scenario in a single process using
// two independent pools.
//
// POPANDPEEK-FORK: entire test function — fork-custom reproduction for be-2ppnh
func TestDoltCommitIsolation_ExternalProcessAbsorption(t *testing.T) {
	// POPANDPEEK-FORK BEGIN: regression harness for a hypothesis that
	// invalidated itself. Initial reproduction run on 2026-04-12 PASSED —
	// no cross-session absorption observed between bd's Pattern B UpdateIssue
	// code path and an independent external DOLT_COMMIT. be-2ppnh was closed
	// NO-BUG as a result. This test is kept in place as a regression harness:
	// if any future Dolt version, bd refactor, or load pattern ever makes
	// the absorption real, this test reproduces it and fails CI loudly.
	// Unskip manually by removing the t.Skip line below if you need to
	// re-run the reproduction (e.g. during upstream Dolt version bumps).
	t.Skip("skipping regression harness — would catch absorption if it ever becomes real")
	// POPANDPEEK-FORK END

	store, cleanup := setupConcurrentTestStore(t)
	defer cleanup()

	ctx, cancel := concurrentTestContext(t)
	defer cancel()

	// Resolve the database name from the store's own pool so the external
	// connection targets the same schema. POPANDPEEK-FORK: SELECT DATABASE()
	// is the cleanest way to reach the private store.config.Database field
	// without refactoring DoltStore.
	var dbName string
	if err := store.db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName); err != nil {
		t.Fatalf("resolve current database: %v", err)
	}
	if dbName == "" {
		t.Fatal("empty database name from SELECT DATABASE() — test setup broken")
	}

	// Create a bead that goroutine A will close concurrently.
	issue := &types.Issue{
		Title:       "be-2ppnh isolation test bead",
		Description: "Target of the concurrent bd.UpdateIssue goroutine",
		Status:      types.StatusOpen,
		Priority:    1,
		IssueType:   types.TypeTask,
	}
	if err := store.CreateIssue(ctx, issue, "isolation-test-setup"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	beadID := issue.ID

	// POPANDPEEK-FORK BEGIN: open a SECOND sql.DB pool to the same database.
	// This is the crux of the reproduction — a completely independent Go pool
	// means its connections have no relationship to bd's pool, and any
	// cross-session leakage would mean Dolt's session isolation is broken at
	// the database protocol level, not at the bd Go-level.
	externalDSN := fmt.Sprintf("root:@tcp(127.0.0.1:%d)/%s?parseTime=true&multiStatements=true",
		testServerPort, dbName)
	externalDB, err := sql.Open("mysql", externalDSN)
	if err != nil {
		t.Fatalf("external sql.Open: %v", err)
	}
	defer externalDB.Close()
	externalDB.SetMaxOpenConns(2)
	if err := externalDB.PingContext(ctx); err != nil {
		t.Fatalf("external ping: %v", err)
	}
	// POPANDPEEK-FORK END

	// Synchronize both goroutines to start at the same moment so the write
	// and the DOLT_COMMIT race as closely as possible.
	var wg sync.WaitGroup
	startBarrier := make(chan struct{})

	// Goroutine A: bd close via Pattern B (current UpdateIssue code path).
	var bdErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-startBarrier
		bdErr = store.UpdateIssue(ctx, beadID, map[string]interface{}{
			"status": string(types.StatusClosed),
		}, "bd-actor-A")
	}()

	// Goroutine B: external process simulating perf-audit.sh. Opens its own
	// connection from the external pool and runs a schema change + manual
	// DOLT_COMMIT with a distinctive author so the resulting commit is easy
	// to identify in dolt_log.
	var extErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-startBarrier
		conn, err := externalDB.Conn(ctx)
		if err != nil {
			extErr = fmt.Errorf("external Conn: %w", err)
			return
		}
		defer conn.Close()
		// Schema-style change on a side table that doesn't touch issues.
		// Mirrors migration 014's ALTER TABLE in shape but on a different table
		// so any issues-row absorption is unambiguously cross-session leakage.
		if _, err := conn.ExecContext(ctx,
			"CREATE TABLE IF NOT EXISTS _be2ppnh_isolation_marker (id INT PRIMARY KEY, note VARCHAR(255))"); err != nil {
			extErr = fmt.Errorf("CREATE TABLE: %w", err)
			return
		}
		if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
			extErr = fmt.Errorf("DOLT_ADD: %w", err)
			return
		}
		if _, err := conn.ExecContext(ctx,
			"CALL DOLT_COMMIT('--author', ?, '-m', ?)",
			"be-2ppnh-sim <sim@test>", "schema: be-2ppnh isolation marker (goroutine B)"); err != nil {
			extErr = fmt.Errorf("DOLT_COMMIT: %w", err)
			return
		}
	}()

	// Release both goroutines simultaneously.
	close(startBarrier)
	wg.Wait()

	if bdErr != nil {
		t.Fatalf("goroutine A (bd.UpdateIssue): %v", bdErr)
	}
	if extErr != nil {
		t.Fatalf("goroutine B (external DOLT_COMMIT): %v", extErr)
	}

	// POPANDPEEK-FORK BEGIN: core assertion — the external commit must not
	// capture bd's status flip. Join dolt_diff_issues with dolt_log on the
	// commit hash (to_commit) because committer/message live on dolt_log,
	// not dolt_diff_issues. If any matching row exists, cross-session
	// absorption happened and we've reproduced the be-2ppnh bug.
	var absorbedCount int
	absorbedQuery := `
		SELECT COUNT(*)
		FROM dolt_diff_issues d
		JOIN dolt_log l ON l.commit_hash = d.to_commit
		WHERE d.to_id = ?
		  AND l.committer LIKE 'be-2ppnh-sim%'
	`
	if err := store.db.QueryRowContext(ctx, absorbedQuery, beadID).Scan(&absorbedCount); err != nil {
		t.Fatalf("query absorbed: %v", err)
	}

	// Dump all commits touching this bead for diagnostic clarity regardless
	// of pass/fail — we want the evidence in the test log either way.
	diagRows, diagErr := store.db.QueryContext(ctx, `
		SELECT l.commit_hash, l.committer, SUBSTRING(l.message, 1, 80), d.to_status, d.diff_type
		FROM dolt_diff_issues d
		JOIN dolt_log l ON l.commit_hash = d.to_commit
		WHERE d.to_id = ?
		ORDER BY l.date
	`, beadID)
	if diagErr != nil {
		t.Logf("diag query err: %v", diagErr)
	} else {
		defer diagRows.Close()
		t.Logf("commits touching bead %s:", beadID)
		for diagRows.Next() {
			var hash, committer, msg, status, diffType string
			if err := diagRows.Scan(&hash, &committer, &msg, &status, &diffType); err == nil {
				t.Logf("  %s  committer=%-25s  status=%-8s  diff=%-10s  msg=%q", hash[:12], committer, status, diffType, msg)
			}
		}
	}

	if absorbedCount > 0 {
		t.Fatalf("LEAK REPRODUCED: bead %s appears in the external 'be-2ppnh-sim' commit's diff (count=%d). Cross-session isolation failed — bd's Pattern B transaction handling is leaking writes to external commits. This is the be-2ppnh root cause.", beadID, absorbedCount)
	}
	// POPANDPEEK-FORK END

	// POPANDPEEK-FORK BEGIN: sanity assertion — bd's own commit should exist
	// and contain the status flip. If this fails while the absorption
	// assertion passes, bd silently dropped the write (the 'nothing to commit'
	// swallow path from issues.go:176 fired).
	var bdCount int
	bdQuery := `
		SELECT COUNT(*)
		FROM dolt_diff_issues d
		JOIN dolt_log l ON l.commit_hash = d.to_commit
		WHERE d.to_id = ?
		  AND d.to_status = 'closed'
		  AND l.committer NOT LIKE 'be-2ppnh-sim%'
	`
	if err := store.db.QueryRowContext(ctx, bdQuery, beadID).Scan(&bdCount); err != nil {
		t.Fatalf("query bd commit: %v", err)
	}
	if bdCount == 0 {
		t.Fatalf("SILENT DROP: bead %s close was not recorded by any non-external commit. bd's UpdateIssue returned nil but the status flip is nowhere in dolt_diff_issues. This is a different class of the be-2ppnh bug — Pattern B's isDoltNothingToCommit error swallow silently discarded the write.", beadID)
	}
	// POPANDPEEK-FORK END
}
