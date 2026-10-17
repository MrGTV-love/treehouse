package pool

import (
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
	if statuses[0].ChangedDuringScan {
		t.Fatal("adding another slot incorrectly marked the unchanged snapshot slot")
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
	if len(statuses) != 1 || statuses[0].Status != StatusLeased || statuses[0].LeaseID != original.LeaseID || statuses[0].LeaseHolder != original.LeaseHolder || !statuses[0].LeasedAt.Equal(original.LeasedAt) || !statuses[0].ChangedDuringScan {
		t.Fatalf("changed slot must retain the entire original lease and be marked: %+v", statuses)
	}
	current := entryFor(t, dir, original.Path)
	if !current.Leased || current.LeaseID != replacement.lease.LeaseID || current.LeaseHolder != replacement.lease.LeaseHolder || !current.LeasedAt.Equal(replacement.lease.LeasedAt) {
		t.Fatalf("status overwrote the replacement lease: %+v", current)
	}
	if _, err := AcquireLeaseInfo(repo, dir, 1, nil, "third"); err == nil {
		t.Fatal("the replacement slot was handed out twice")
	}
	if err := ReleaseConditional(dir, original.Path, "", ReleasePreconditions{ExpectedLeaseID: &statuses[0].LeaseID}, nil); err == nil {
		t.Fatal("a stale snapshot released the replacement lease")
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
	if len(statuses) != 1 || statuses[0].Path != path || !statuses[0].ChangedDuringScan {
		t.Fatalf("removed slot was not retained and marked in the snapshot: %+v", statuses)
	}
	state, err := ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Worktrees) != 0 {
		t.Fatalf("status resurrected a pruned slot: %+v", state)
	}
}

func TestListRecoveryDoesNotOverwriteConcurrentLease(t *testing.T) {
	_, dir, path := recoveredFixture(t)
	finish, resume := blockStatusScan(t, dir)
	type outcome struct {
		lease LeaseInfo
		err   error
	}
	done := make(chan outcome, 1)
	go func() { lease, err := LeaseExisting(dir, "1", "replacement"); done <- outcome{lease, err} }()
	var replacement outcome
	select {
	case replacement = <-done:
	case <-time.After(10 * time.Second):
		resume()
		<-done
		t.Fatal("lease waited for status's quarantined-entry process scan")
	}
	if replacement.err != nil {
		t.Fatal(replacement.err)
	}
	statuses := finish()
	if len(statuses) != 1 || statuses[0].LeaseHolder != RecoveredLeaseHolder || !statuses[0].ChangedDuringScan {
		t.Fatalf("recovery must retain and mark the old quarantine snapshot: %+v", statuses)
	}
	current := entryFor(t, dir, path)
	if !current.Leased || current.LeaseID != replacement.lease.LeaseID || current.LeaseHolder != replacement.lease.LeaseHolder {
		t.Fatalf("status's stale recovery cleared or mixed the replacement lease: %+v", current)
	}
}
