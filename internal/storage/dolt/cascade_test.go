// POPANDPEEK-FORK BEGIN: cascade + cycle-detection test suite (be-merbj.3)
package dolt

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// mkIssue is a small helper that keeps the per-test boilerplate minimal.
func mkIssue(id, title string) *types.Issue {
	return &types.Issue{
		ID:        id,
		Title:     title,
		Status:    types.StatusOpen,
		Priority:  2,
		IssueType: types.TypeTask,
	}
}

func TestCloseIssue_CascadesToChildren(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	parent := mkIssue("casc-parent", "Parent")
	child1 := mkIssue("casc-child-1", "Child 1")
	child2 := mkIssue("casc-child-2", "Child 2")
	grand := mkIssue("casc-grand", "Grandchild")

	for _, iss := range []*types.Issue{parent, child1, child2, grand} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create %s: %v", iss.ID, err)
		}
	}

	// child1, child2 -> parent; grand -> child1
	links := []*types.Dependency{
		{IssueID: child1.ID, DependsOnID: parent.ID, Type: types.DepParentChild},
		{IssueID: child2.ID, DependsOnID: parent.ID, Type: types.DepParentChild},
		{IssueID: grand.ID, DependsOnID: child1.ID, Type: types.DepParentChild},
	}
	for _, d := range links {
		if err := store.AddDependency(ctx, d, "tester"); err != nil {
			t.Fatalf("add dep %s->%s: %v", d.IssueID, d.DependsOnID, err)
		}
	}

	if err := store.CloseIssue(ctx, parent.ID, "epic shipped", "tester", "sess-1"); err != nil {
		t.Fatalf("CloseIssue parent: %v", err)
	}

	// All descendants closed, each with "via parent <parent.ID>" suffix.
	for _, id := range []string{parent.ID, child1.ID, child2.ID, grand.ID} {
		got, err := store.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != types.StatusClosed {
			t.Errorf("%s status = %q, want closed", id, got.Status)
		}
	}

	// Parent retains original reason; descendants carry the suffix.
	p, _ := store.GetIssue(ctx, parent.ID)
	if p.CloseReason != "epic shipped" {
		t.Errorf("parent close_reason = %q, want %q", p.CloseReason, "epic shipped")
	}

	c1, _ := store.GetIssue(ctx, child1.ID)
	wantSuffix := "via parent " + parent.ID
	if !strings.Contains(c1.CloseReason, wantSuffix) {
		t.Errorf("child1 close_reason = %q, want suffix %q", c1.CloseReason, wantSuffix)
	}
	if !strings.Contains(c1.CloseReason, "epic shipped") {
		t.Errorf("child1 close_reason = %q, want to include parent reason", c1.CloseReason)
	}

	g, _ := store.GetIssue(ctx, grand.ID)
	if !strings.Contains(g.CloseReason, wantSuffix) {
		t.Errorf("grand close_reason = %q, want suffix %q", g.CloseReason, wantSuffix)
	}
}

func TestCloseIssue_CascadeSkipsAlreadyClosed(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	parent := mkIssue("skip-parent", "Parent")
	child := mkIssue("skip-child", "Child")

	for _, iss := range []*types.Issue{parent, child} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create %s: %v", iss.ID, err)
		}
	}
	dep := &types.Dependency{IssueID: child.ID, DependsOnID: parent.ID, Type: types.DepParentChild}
	if err := store.AddDependency(ctx, dep, "tester"); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	// Close the child directly first, with its own independent reason.
	if err := store.CloseIssue(ctx, child.ID, "finished early", "tester", "sess-early"); err != nil {
		t.Fatalf("close child early: %v", err)
	}

	// Now close the parent — the cascade must NOT overwrite the child's
	// pre-existing close reason, because the child is already closed.
	if err := store.CloseIssue(ctx, parent.ID, "parent done", "tester", "sess-later"); err != nil {
		t.Fatalf("close parent: %v", err)
	}

	c, _ := store.GetIssue(ctx, child.ID)
	if c.CloseReason != "finished early" {
		t.Errorf("child close_reason = %q, want untouched %q", c.CloseReason, "finished early")
	}
}

func TestCloseIssue_NoChildren(t *testing.T) {
	// Cascade is a no-op for a leaf bead; exercising the path ensures the
	// Pattern A wrapper does not regress the happy path.
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	leaf := mkIssue("leaf-only", "Leaf")
	if err := store.CreateIssue(ctx, leaf, "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := store.CloseIssue(ctx, leaf.ID, "direct close", "tester", "sess"); err != nil {
		t.Fatalf("close: %v", err)
	}
	got, _ := store.GetIssue(ctx, leaf.ID)
	if got.Status != types.StatusClosed {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if got.CloseReason != "direct close" {
		t.Fatalf("close_reason = %q, want 'direct close'", got.CloseReason)
	}
}

func TestUpdateIssue_CascadesStatusAndExternalRef(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	parent := mkIssue("upd-parent", "Parent")
	child1 := mkIssue("upd-child-1", "Child 1")
	child2 := mkIssue("upd-child-2", "Child 2")

	for _, iss := range []*types.Issue{parent, child1, child2} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create %s: %v", iss.ID, err)
		}
	}
	for _, c := range []*types.Issue{child1, child2} {
		d := &types.Dependency{IssueID: c.ID, DependsOnID: parent.ID, Type: types.DepParentChild}
		if err := store.AddDependency(ctx, d, "tester"); err != nil {
			t.Fatalf("add dep: %v", err)
		}
	}

	// Cascade status + external_ref; title must NOT cascade.
	err := store.UpdateIssue(ctx, parent.ID, map[string]interface{}{
		"status":       string(types.StatusWorking),
		"external_ref": "gh:42",
		"title":        "Parent renamed",
	}, "tester")
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}

	for _, id := range []string{child1.ID, child2.ID} {
		got, err := store.GetIssue(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != types.StatusWorking {
			t.Errorf("%s status = %q, want working", id, got.Status)
		}
		if got.ExternalRef == nil || *got.ExternalRef != "gh:42" {
			ref := "<nil>"
			if got.ExternalRef != nil {
				ref = *got.ExternalRef
			}
			t.Errorf("%s external_ref = %q, want gh:42", id, ref)
		}
		// Title should be the original — must not cascade.
		wantTitle := "Child 1"
		if id == child2.ID {
			wantTitle = "Child 2"
		}
		if got.Title != wantTitle {
			t.Errorf("%s title = %q, want unchanged %q", id, got.Title, wantTitle)
		}
	}
}

func TestUpdateIssue_NonCascadableFieldsAreLocal(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	parent := mkIssue("loc-parent", "Parent")
	child := mkIssue("loc-child", "Child")
	for _, iss := range []*types.Issue{parent, child} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	d := &types.Dependency{IssueID: child.ID, DependsOnID: parent.ID, Type: types.DepParentChild}
	if err := store.AddDependency(ctx, d, "tester"); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	// Only non-cascadable fields — cascade must be a no-op.
	err := store.UpdateIssue(ctx, parent.ID, map[string]interface{}{
		"title":    "Parent renamed",
		"priority": 1,
	}, "tester")
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}
	c, _ := store.GetIssue(ctx, child.ID)
	if c.Title != "Child" {
		t.Errorf("child title = %q, want unchanged %q", c.Title, "Child")
	}
	if c.Priority != 2 {
		t.Errorf("child priority = %d, want unchanged 2", c.Priority)
	}
}

func TestUpdateIssue_StatusClosedAddsViaParentSuffix(t *testing.T) {
	// Closing via `bd update --status closed` (as opposed to `bd close`) must
	// also suffix the cascaded reason with "via parent <id>", per acceptance
	// criteria. Covers the UpdateIssue path through cascadeUpdateToDescendants.
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	parent := mkIssue("uc-parent", "Parent")
	child := mkIssue("uc-child", "Child")
	for _, iss := range []*types.Issue{parent, child} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}
	d := &types.Dependency{IssueID: child.ID, DependsOnID: parent.ID, Type: types.DepParentChild}
	if err := store.AddDependency(ctx, d, "tester"); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	err := store.UpdateIssue(ctx, parent.ID, map[string]interface{}{
		"status":       string(types.StatusClosed),
		"close_reason": "shipped",
	}, "tester")
	if err != nil {
		t.Fatalf("UpdateIssue: %v", err)
	}

	c, _ := store.GetIssue(ctx, child.ID)
	if c.Status != types.StatusClosed {
		t.Errorf("child status = %q, want closed", c.Status)
	}
	if !strings.Contains(c.CloseReason, "via parent "+parent.ID) {
		t.Errorf("child close_reason = %q, want suffix 'via parent %s'", c.CloseReason, parent.ID)
	}
	if !strings.Contains(c.CloseReason, "shipped") {
		t.Errorf("child close_reason = %q, want to include parent reason 'shipped'", c.CloseReason)
	}
}

func TestAddDependency_RejectsSelfParent(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	a := mkIssue("self-a", "A")
	if err := store.CreateIssue(ctx, a, "tester"); err != nil {
		t.Fatalf("create: %v", err)
	}

	dep := &types.Dependency{IssueID: a.ID, DependsOnID: a.ID, Type: types.DepParentChild}
	err := store.AddDependency(ctx, dep, "tester")
	if err == nil {
		t.Fatal("expected ErrParentCycle for self-parent, got nil")
	}
	if !errors.Is(err, ErrParentCycle) {
		t.Fatalf("error is not ErrParentCycle: %v", err)
	}
}

func TestAddDependency_RejectsDirectParentCycle(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	a := mkIssue("dir-a", "A")
	b := mkIssue("dir-b", "B")
	for _, iss := range []*types.Issue{a, b} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// Establish A.parent = B
	first := &types.Dependency{IssueID: a.ID, DependsOnID: b.ID, Type: types.DepParentChild}
	if err := store.AddDependency(ctx, first, "tester"); err != nil {
		t.Fatalf("first add: %v", err)
	}

	// Now B.parent = A — forms loop A<->B
	loop := &types.Dependency{IssueID: b.ID, DependsOnID: a.ID, Type: types.DepParentChild}
	err := store.AddDependency(ctx, loop, "tester")
	if err == nil {
		t.Fatal("expected ErrParentCycle for A<->B loop, got nil")
	}
	if !errors.Is(err, ErrParentCycle) {
		t.Fatalf("error is not ErrParentCycle: %v", err)
	}
}

func TestAddDependency_RejectsIndirectParentCycle(t *testing.T) {
	// A.parent=B, B.parent=C, then C.parent=A would close a three-node loop.
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	a := mkIssue("ind-a", "A")
	b := mkIssue("ind-b", "B")
	c := mkIssue("ind-c", "C")
	for _, iss := range []*types.Issue{a, b, c} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	for _, d := range []*types.Dependency{
		{IssueID: a.ID, DependsOnID: b.ID, Type: types.DepParentChild},
		{IssueID: b.ID, DependsOnID: c.ID, Type: types.DepParentChild},
	} {
		if err := store.AddDependency(ctx, d, "tester"); err != nil {
			t.Fatalf("add dep: %v", err)
		}
	}

	closing := &types.Dependency{IssueID: c.ID, DependsOnID: a.ID, Type: types.DepParentChild}
	err := store.AddDependency(ctx, closing, "tester")
	if err == nil {
		t.Fatal("expected ErrParentCycle for 3-node loop, got nil")
	}
	if !errors.Is(err, ErrParentCycle) {
		t.Fatalf("error is not ErrParentCycle: %v", err)
	}
}

func TestAddDependency_AllowsLinearParentChain(t *testing.T) {
	// Sanity check: a plain A.parent=B.parent=C chain must not trigger the
	// cycle detector. Guards against false positives.
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	a := mkIssue("ok-a", "A")
	b := mkIssue("ok-b", "B")
	c := mkIssue("ok-c", "C")
	for _, iss := range []*types.Issue{a, b, c} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	for _, d := range []*types.Dependency{
		{IssueID: a.ID, DependsOnID: b.ID, Type: types.DepParentChild},
		{IssueID: b.ID, DependsOnID: c.ID, Type: types.DepParentChild},
	} {
		if err := store.AddDependency(ctx, d, "tester"); err != nil {
			t.Fatalf("unexpected error adding linear chain dep: %v", err)
		}
	}
}

func TestAddDependency_NonParentChildTypeBypassesCycleCheck(t *testing.T) {
	// Non-parent-child dependencies (blocks, related, etc.) must not be
	// routed through the parent-cycle detector.
	store, cleanup := setupTestStore(t)
	defer cleanup()

	ctx, cancel := testContext(t)
	defer cancel()

	a := mkIssue("blk-a", "A")
	b := mkIssue("blk-b", "B")
	for _, iss := range []*types.Issue{a, b} {
		if err := store.CreateIssue(ctx, iss, "tester"); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// Parent chain A -> B
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID: a.ID, DependsOnID: b.ID, Type: types.DepParentChild,
	}, "tester"); err != nil {
		t.Fatalf("add parent dep: %v", err)
	}

	// A "blocks" dep B -> A should NOT be rejected — it isn't a parent edge.
	if err := store.AddDependency(ctx, &types.Dependency{
		IssueID: b.ID, DependsOnID: a.ID, Type: types.DepBlocks,
	}, "tester"); err != nil {
		t.Fatalf("blocks dep should be allowed even though it would close a parent cycle if it were parent-child: %v", err)
	}
}

// POPANDPEEK-FORK END
