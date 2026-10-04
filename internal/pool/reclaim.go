package pool

import (
	"errors"
	"fmt"
	"os"

	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

// reclaimForeignWorktree runs only at the cap, after own-clone reuse failed,
// while the acquisition holds the state lock. It frees at most one slot; the
// caller then uses its ordinary fresh-allocation path, never the foreign tree.
func reclaimForeignWorktree(poolDir string, state *State, requester os.FileInfo, skipFetch bool) error {
	for i, wt := range state.Worktrees {
		if wt.Destroying || wt.Leased || ownerAlive(wt) || !wt.SeedInventoryKnown || vcs.WorktreeBackendName(wt.Path) != "git" {
			continue
		}
		owner, err := acquisitionCommonGitDir(wt.Path)
		if err != nil || os.SameFile(owner, requester) {
			continue
		}
		processes, err := findProcessesInWorktree(wt.Path)
		if err != nil || len(processes) != 0 {
			continue
		}
		untracked, reason := vcs.RecoveryWorktree(wt.Path)
		if reason != "" || len(untracked) != 0 {
			continue
		}
		ownerRoot, err := resolvePoolRepoRoot(wt)
		if err != nil {
			continue
		}
		rootIdentity, err := acquisitionCommonGitDir(ownerRoot)
		if err != nil || !os.SameFile(rootIdentity, owner) {
			continue
		}
		if !skipFetch {
			if err := vcs.Fetch(ownerRoot); err != nil {
				continue
			}
		}
		container, err := removableWorktreeContainer(poolDir, wt.Path)
		if err != nil {
			continue
		}
		err = vcs.RemoveLandedWorktree(ownerRoot, wt.Path, func() error {
			// Do not drop the caller or its ancestors: a shell standing in a
			// foreign slot is real usage, even when it holds no durable lease.
			processes, err := findProcessesInWorktree(wt.Path)
			if err != nil {
				return err
			}
			if len(processes) != 0 {
				return fmt.Errorf("a process entered worktree %s", wt.Name)
			}
			current, err := acquisitionCommonGitDir(wt.Path)
			if err != nil || !os.SameFile(current, owner) {
				return fmt.Errorf("worktree %s ownership changed", wt.Name)
			}
			return nil
		})
		if errors.Is(err, vcs.ErrWorktreeNotDisposable) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot reclaim foreign worktree %s: %w", wt.Name, err)
		}
		// Commit the deletion before creation: a failed add cannot leave the
		// removed foreign registration consuming the shared budget in state.
		state.Worktrees = append(state.Worktrees[:i], state.Worktrees[i+1:]...)
		if err := persistState(poolDir, *state); err != nil {
			return fmt.Errorf("foreign worktree %s removed but state update failed: %w", wt.Name, err)
		}
		if container != wt.Path {
			// Only remove an empty pool-owned container. Files beside the old
			// worktree are not its contents and must never be deleted here.
			if err := os.Remove(container); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "treehouse: warning: reclaimed worktree %s but kept its slot directory: %v\n", wt.Name, err)
			}
		}
		fmt.Fprintf(os.Stderr, "treehouse: reclaimed idle foreign-clone worktree %s for this clone.\n", wt.Name)
		return nil
	}
	return nil
}
