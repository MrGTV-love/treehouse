package pool

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/kunchenguid/treehouse/v3/internal/vcs"
)

// reclaimForeignWorktree runs only at the cap, after own-clone reuse failed.
// Each candidate has its own fence and reservation; no VCS or process work
// holds the pool lock. The removal and the caller's fresh claim are one state
// write; the caller creates its own tree, never reuses a foreign one.
func reclaimForeignWorktree(repoRoot, poolDir string, poolSize, firstSlot int, snapshot State, candidates []int, requester os.FileInfo, opts acquireOptions, report func(string, string, string)) ([]string, *acquisitionClaim, error) {
	acquisitionStage("foreign-snapshot", poolDir)
	selection, scanErr := snapshotWorktreeProcesses()
	var refusals []string
	var fresh *acquisitionClaim
	for _, index := range candidates {
		wt := snapshot.Worktrees[index]
		if wt.OwnerPID != 0 && !wt.Destroying && !wt.Leased {
			current, err := currentCandidate(poolDir, wt)
			if err != nil {
				report(wt.Path, "refused", "foreign reservation could not be read")
				return refusals, nil, err
			}
			wt = current
		}
		reported := false
		skip := func(reason, summary string) {
			refusals = append(refusals, fmt.Sprintf("worktree %s: %s", wt.Name, reason))
			report(wt.Path, "refused", summary)
			reported = true
		}
		switch {
		case wt.Destroying:
			skip("destruction is in progress", "destruction in progress")
			continue
		case wt.Leased:
			skip("durably leased", "durably leased")
			continue
		case ownerAlive(wt):
			skip("has a live owner reservation", "live owner reservation")
			continue
		case !wt.SeedInventoryKnown:
			skip("seed inventory cannot be verified", "seed inventory unavailable")
			continue
		case vcs.WorktreeBackendName(wt.Path) != "git":
			skip("not a readable Git worktree", "unreadable Git worktree")
			continue
		}
		claim, err := claimCandidate(poolDir, wt)
		if err != nil {
			report(wt.Path, "refused", "foreign reservation could not be saved")
			return refusals, nil, err
		}
		if claim == nil {
			skip("reservation changed or acquisition in progress", "reservation changed or acquisition in progress")
			continue
		}
		removed := false
		err = func() (resultErr error) {
			defer claim.close()
			defer func() {
				if !removed {
					if err := claim.save(State{Worktrees: []WorktreeEntry{wt}}); resultErr == nil {
						resultErr = err
					}
				}
			}()
			acquisitionStage("screening", wt.Path)
			owner, err := acquisitionCommonGitDir(wt.Path)
			if err != nil {
				skip(fmt.Sprintf("clone identity cannot be verified: %v", err), "clone identity unavailable")
				return nil
			}
			if os.SameFile(owner, requester) {
				skip("belongs to the requesting clone", "belongs to requesting clone")
				return nil
			}
			processes, err := selection.InWorktree(wt.Path)
			if scanErr != nil {
				err = scanErr
			} else if err == nil && len(processes) != 0 {
				processes, err = findProcessesInWorktree(wt.Path)
			}
			if err != nil {
				skip(fmt.Sprintf("process table cannot be read: %v", err), "process snapshot unavailable")
				return nil
			}
			if len(processes) != 0 {
				skip("has live processes", "live process")
				return nil
			}
			untracked, reason := vcs.RecoveryWorktree(wt.Path)
			if reason != "" {
				skip(reason, "foreign checkout failed safety checks")
				return nil
			}
			if len(untracked) != 0 {
				skip("has untracked paths: "+strings.Join(untracked, ", "), "foreign copy has untracked paths")
				return nil
			}
			ownerRoot, err := resolvePoolRepoRoot(wt)
			if err != nil {
				skip(fmt.Sprintf("owning repository cannot be resolved: %v", err), "owning repository unavailable")
				return nil
			}
			rootIdentity, err := acquisitionCommonGitDir(ownerRoot)
			if err != nil {
				skip(fmt.Sprintf("owning repository identity cannot be verified: %v", err), "owning repository identity unavailable")
				return nil
			}
			if !os.SameFile(rootIdentity, owner) {
				skip("owning repository identity does not match the slot", "owning repository identity mismatch")
				return nil
			}
			if !opts.skipFetch {
				if err := vcs.Fetch(ownerRoot); err != nil {
					skip(fmt.Sprintf("owning repository fetch failed: %v", err), "owning repository fetch failed")
					return nil
				}
			}
			container, err := removableWorktreeContainer(poolDir, wt.Path)
			if err != nil {
				skip(err.Error(), "foreign copy failed safe-removal checks")
				return nil
			}
			acquisitionStage("foreign-validated", wt.Path)
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
				skip(err.Error(), "foreign copy failed safe-removal checks")
				return nil
			}
			if err != nil {
				return fmt.Errorf("cannot reclaim foreign worktree %s: %w", wt.Name, err)
			}
			// Commit the deletion before creation: a failed add cannot leave the
			// removed foreign registration consuming the shared budget in state.
			removed = true
			if container != wt.Path {
				// Only remove an empty pool-owned container. Files beside the old
				// worktree are not its contents and must never be deleted here.
				if err := os.Remove(container); err != nil && !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "treehouse: warning: reclaimed worktree %s but kept its slot directory: %v\n", wt.Name, err)
				}
			}
			fresh, err = claim.replaceWithFresh(repoRoot, poolSize, firstSlot, opts)
			if err != nil {
				return fmt.Errorf("foreign worktree %s removed but state update failed: %w", wt.Name, err)
			}
			fmt.Fprintf(os.Stderr, "treehouse: reclaimed idle foreign-clone worktree %s for this clone.\n", wt.Name)
			return nil
		}()
		if err != nil {
			if !reported {
				report(wt.Path, "refused", "foreign reclamation failed")
			}
			return refusals, nil, err
		}
		if removed {
			report(wt.Path, "accepted", "reclaimed safe foreign copy")
			return refusals, fresh, nil
		}
	}
	return refusals, nil, nil
}
