package pool

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/process"
)

// Block only the status scan; lifecycle operations still get an immediate,
// successful process-table read. Cleanup always releases and joins List before
// restoring the package seam, including when a lock regression fails the test.
func blockStatusScan(t *testing.T, poolDir string) (func() []WorktreeStatus, func()) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var unblock sync.Once
	old := findProcessesInWorktree
	findProcessesInWorktree = func(string) ([]process.ProcessInfo, error) {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-resume
		}
		return nil, nil
	}
	type outcome struct {
		statuses []WorktreeStatus
		err      error
	}
	done := make(chan outcome, 1)
	go func() { statuses, err := List(poolDir); done <- outcome{statuses, err} }()
	release := func() { unblock.Do(func() { close(resume) }) }
	var result outcome
	var join sync.Once
	finish := func() []WorktreeStatus {
		t.Helper()
		release()
		join.Do(func() { result = <-done })
		if result.err != nil {
			t.Errorf("List: %v", result.err)
		}
		return result.statuses
	}
	t.Cleanup(func() { finish(); findProcessesInWorktree = old })
	select {
	case <-entered:
	case result = <-done:
		join.Do(func() {})
		t.Fatalf("List finished before scanning: %v", result.err)
	case <-time.After(10 * time.Second):
		t.Fatal("List did not start its process scan")
	}
	return finish, release
}

func TestListDoesNotBlockAcquire(t *testing.T) {
	repo, dir := setupLocalRepo(t)
	original, err := AcquireLeaseInfo(repo, dir, 2, nil, "original")
	if err != nil {
		t.Fatal(err)
	}
	finish, resume := blockStatusScan(t, dir)
	type acquisition struct {
		lease LeaseInfo
		err   error
	}
	done := make(chan acquisition, 1)
	go func() {
		lease, err := AcquireLeaseInfo(repo, dir, 2, nil, "concurrent")
		done <- acquisition{lease, err}
	}()
	var acquired acquisition
	select {
	case acquired = <-done:
	case <-time.After(10 * time.Second):
		resume()
		<-done
		t.Fatal("get waited for the blocked status process scan")
	}
	if acquired.err != nil {
		t.Fatal(acquired.err)
	}
	if acquired.lease.Path == original.Path {
		t.Fatal("get handed out an already leased slot")
	}
	statuses := finish()
	if len(statuses) != 1 || statuses[0].LeaseID != original.LeaseID || statuses[0].LeaseHolder != original.LeaseHolder || !statuses[0].LeasedAt.Equal(original.LeasedAt) {
		t.Fatalf("status lost its snapshot lease: %+v", statuses)
	}
	current, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Worktrees) != 2 {
		t.Fatalf("status overwrote concurrent acquisition: %+v", current)
	}
	for _, wt := range current.Worktrees {
		if !wt.Leased || wt.LeaseID == "" || wt.LeaseHolder == "" {
			t.Fatalf("inconsistent lease: %+v", wt)
		}
	}
}

func TestListSnapshotSurvivesReturnAndReacquire(t *testing.T) {
	repo, dir := setupLocalRepo(t)
	original, err := AcquireLeaseInfo(repo, dir, 1, nil, "original")
	if err != nil {
		t.Fatal(err)
	}
	finish, resume := blockStatusScan(t, dir)
	type outcome struct {
		lease LeaseInfo
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		if err := ReleaseConditional(dir, original.Path, "", ReleasePreconditions{ExpectedLeaseID: &original.LeaseID}, nil); err != nil {
			done <- outcome{err: err}
			return
		}
		lease, err := AcquireLeaseInfo(repo, dir, 1, nil, "replacement")
		done <- outcome{lease, err}
	}()
	var replacement outcome
	select {
	case replacement = <-done:
	case <-time.After(10 * time.Second):
		resume()
		<-done
		t.Fatal("return/get waited for the blocked status process scan")
	}
	if replacement.err != nil {
		t.Fatal(replacement.err)
	}
	if replacement.lease.Path != original.Path || replacement.lease.LeaseID == original.LeaseID {
		t.Fatalf("expected a new lease on the returned slot: %+v", replacement.lease)
	}
	statuses := finish()
	if len(statuses) != 1 || statuses[0].Status != StatusLeased || statuses[0].LeaseID != original.LeaseID || statuses[0].LeaseHolder != original.LeaseHolder || !statuses[0].LeasedAt.Equal(original.LeasedAt) {
		t.Fatalf("changed slot must retain the entire original lease: %+v", statuses)
	}
	current := entryFor(t, dir, original.Path)
	if !current.Leased || current.LeaseID != replacement.lease.LeaseID || current.LeaseHolder != replacement.lease.LeaseHolder || !current.LeasedAt.Equal(replacement.lease.LeasedAt) {
		t.Fatalf("status overwrote the replacement lease: %+v", current)
	}
	if _, err := AcquireLeaseInfo(repo, dir, 1, nil, "third"); err == nil {
		t.Fatal("the replacement slot was handed out twice")
	}
	if err := ReleaseConditional(dir, original.Path, "", ReleasePreconditions{ExpectedReservation: &statuses[0].Reservation}, nil); !errors.Is(err, ErrLeasePreconditionFailed) {
		t.Fatalf("a stale snapshot released the replacement lease: %v", err)
	}
}

func TestListDoesNotBlockPrune(t *testing.T) {
	repo, dir := setupLocalRepo(t)
	path := idleSlots(t, repo, dir, 1)[0]
	finish, resume := blockStatusScan(t, dir)
	type outcome struct {
		result PruneResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := Prune(repo, dir, false, nil); done <- outcome{result, err} }()
	var pruned outcome
	select {
	case pruned = <-done:
	case <-time.After(10 * time.Second):
		resume()
		<-done
		t.Fatal("prune waited for the blocked status process scan")
	}
	if pruned.err != nil {
		t.Fatal(pruned.err)
	}
	if len(pruned.result.Pruned) != 1 || pruned.result.Pruned[0].Path != path {
		t.Fatalf("prune did not remove the disposable slot: %+v", pruned.result)
	}
	statuses := finish()
	if len(statuses) != 1 || statuses[0].Path != path {
		t.Fatalf("removed slot was not retained in the snapshot: %+v", statuses)
	}
	state, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Worktrees) != 0 {
		t.Fatalf("status resurrected a pruned slot: %+v", state)
	}
}

func mutateEntry(t *testing.T, poolDir, path string, change func(*WorktreeEntry)) {
	t.Helper()
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range state.Worktrees {
		if state.Worktrees[i].Path == path {
			change(&state.Worktrees[i])
			if err := WriteState(poolDir, state); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("worktree %s missing from state %#v", path, state.Worktrees)
}

// A listing can be as old as its whole scan, so a release that acts on a row
// must be refused once the slot's claim differs from the one the listing saw:
// a plain get reserving an unowned slot, a different session owning it, and a
// legacy lease with no ID being re-leased.
func TestExpectedReservationRefusesAClaimMadeAfterTheListing(t *testing.T) {
	repo, dir := setupLocalRepo(t)
	paths := idleSlots(t, repo, dir, 3)
	unowned, owned, legacy := paths[0], paths[1], paths[2]
	mutateEntry(t, dir, owned, func(wt *WorktreeEntry) {
		if err := reserveOwner(wt); err != nil {
			t.Fatal(err)
		}
	})
	mutateEntry(t, dir, legacy, func(wt *WorktreeEntry) {
		wt.Leased, wt.LeaseID, wt.LeaseHolder, wt.LeasedAt = true, "", "legacy", time.Now()
	})

	statuses, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	observed := map[string]Reservation{}
	for _, status := range statuses {
		observed[status.Path] = status.Reservation
	}
	if len(observed) != 3 {
		t.Fatalf("expected all three slots listed: %+v", statuses)
	}
	validate := func(path string) error {
		r := observed[path]
		return ValidateReleasePreconditions(dir, path, ReleasePreconditions{ExpectedReservation: &r}, nil)
	}
	for _, path := range paths {
		if err := validate(path); err != nil {
			t.Fatalf("the claim the listing saw on %s must still release: %v", path, err)
		}
	}

	mutateEntry(t, dir, unowned, func(wt *WorktreeEntry) {
		if err := reserveOwner(wt); err != nil {
			t.Fatal(err)
		}
	})
	mutateEntry(t, dir, owned, func(wt *WorktreeEntry) { wt.OwnerPID++; wt.OwnerStartedAt++ })
	mutateEntry(t, dir, legacy, func(wt *WorktreeEntry) { wt.LeaseID = "re-leased" })
	for name, path := range map[string]string{"plain get": unowned, "other session": owned, "re-lease": legacy} {
		before := entryFor(t, dir, path)
		if err := validate(path); !errors.Is(err, ErrLeasePreconditionFailed) {
			t.Fatalf("%s: validate = %v, want a lease precondition failure", name, err)
		}
		r := observed[path]
		if err := ReleaseConditional(dir, path, "", ReleasePreconditions{ExpectedReservation: &r}, nil); !errors.Is(err, ErrLeasePreconditionFailed) {
			t.Fatalf("%s: release = %v, want a lease precondition failure", name, err)
		}
		if after := entryFor(t, dir, path); !reflect.DeepEqual(before, after) {
			t.Fatalf("%s: a refused release changed the entry:\nbefore %+v\nafter  %+v", name, before, after)
		}
	}
}
