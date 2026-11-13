package pool

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

// reclaimForeignWorktree runs only at the cap, after own-clone reuse failed,
// while the acquisition holds the state lock. It frees at most one slot; the
// caller then uses its ordinary fresh-allocation path, never the foreign tree.
// Refusals are diagnostic only: each skipped slot contributes its first reason,
// in scan order, without retrying or changing any removal precondition.
func reclaimForeignWorktree(poolDir string, state *State, requester os.FileInfo, skipFetch bool) ([]string, error) {
	var refusals []string
	for i, wt := range state.Worktrees {
		skip := func(reason string) {
			refusals = append(refusals, fmt.Sprintf("worktree %s: %s", wt.Name, reason))
		}
		switch {
		case wt.Destroying:
			skip("destruction is in progress")
			continue
		case wt.Leased:
			skip("durably leased")
			continue
		case ownerAlive(wt):
			skip("has a live owner reservation")
			continue
		case !wt.SeedInventoryKnown:
			skip("seed inventory cannot be verified")
			continue
		case vcs.WorktreeBackendName(wt.Path) != "git":
			skip("not a readable Git worktree")
			continue
		}
		owner, err := acquisitionCommonGitDir(wt.Path)
		if err != nil {
			skip(fmt.Sprintf("clone identity cannot be verified: %v", err))
			continue
		}
		if os.SameFile(owner, requester) {
			skip("belongs to the requesting clone")
			continue
		}
		processes, err := findProcessesInWorktree(wt.Path)
		if err != nil {
			skip(fmt.Sprintf("process table cannot be read: %v", err))
			continue
		}
		if len(processes) != 0 {
			skip("has live processes")
			continue
		}
		untracked, reason := vcs.RecoveryWorktree(wt.Path)
		if reason != "" {
			skip(reason)
			continue
		}
		if len(untracked) != 0 {
			skip("has untracked paths: " + strings.Join(untracked, ", "))
			continue
		}
		ownerRoot, err := resolvePoolRepoRoot(wt)
		if err != nil {
			skip(fmt.Sprintf("owning repository cannot be resolved: %v", err))
			continue
		}
		rootIdentity, err := acquisitionCommonGitDir(ownerRoot)
		if err != nil {
			skip(fmt.Sprintf("owning repository identity cannot be verified: %v", err))
			continue
		}
		if !os.SameFile(rootIdentity, owner) {
			skip("owning repository identity does not match the slot")
			continue
		}
		if !skipFetch {
			if err := vcs.Fetch(ownerRoot); err != nil {
				skip(fmt.Sprintf("owning repository fetch failed: %v", err))
				continue
			}
		}
		container, err := removableWorktreeContainer(poolDir, wt.Path)
		if err != nil {
			skip(err.Error())
			continue
		}
		err = vcs.RemoveLandedWorktree(ownerRoot, wt.Path, wt.SeededPaths, func() error {
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
			skip(err.Error())
			continue
		}
		if err != nil {
			return refusals, fmt.Errorf("cannot reclaim foreign worktree %s: %w", wt.Name, err)
		}
		// Commit the deletion before creation: a failed add cannot leave the
		// removed foreign registration consuming the shared budget in state.
		state.Worktrees = append(state.Worktrees[:i], state.Worktrees[i+1:]...)
		if err := persistState(poolDir, *state); err != nil {
			return refusals, fmt.Errorf("foreign worktree %s removed but state update failed: %w", wt.Name, err)
		}
		if container != wt.Path {
			// Only remove an empty pool-owned container. Files beside the old
			// worktree are not its contents and must never be deleted here.
			if err := os.Remove(container); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "treehouse: warning: reclaimed worktree %s but kept its slot directory: %v\n", wt.Name, err)
			}
		}
		fmt.Fprintf(os.Stderr, "treehouse: reclaimed idle foreign-clone worktree %s for this clone.\n", wt.Name)
		return refusals, nil
	}
	return refusals, nil
}
