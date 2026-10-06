package pool

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupPreservesSiblingWorktree(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation string
		recovered bool
		caseOnly  bool
	}{
		{name: "destroy_authenticated_leased", operation: "destroy"},
		{name: "destroy_recovered_dirty", operation: "destroy", recovered: true},
		{name: "prune_protected", operation: "prune"},
		{name: "orphan_prune_leased", operation: "orphan"},
		{name: "destroy_case_only_leased", operation: "destroy", caseOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir, poolDir := setupRepo(t)
			target := acquireDisposable(t, repoDir, poolDir)
			state, err := ReadState(poolDir)
			if err != nil {
				t.Fatal(err)
			}
			siblingName := "sibling"
			if tc.caseOnly {
				siblingName = strings.ToUpper(filepath.Base(target))
				if siblingName == filepath.Base(target) {
					siblingName = strings.ToLower(filepath.Base(target))
				}
				if siblingName == filepath.Base(target) {
					t.Skip("target name has no case variant")
				}
			}
			sibling := filepath.Join(filepath.Dir(target), siblingName)
			if tc.caseOnly {
				if _, err := os.Stat(sibling); err == nil {
					t.Skip("filesystem does not distinguish case-only sibling paths")
				} else if !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			runGit(t, repoDir, "worktree", "add", "--detach", sibling, "HEAD")
			const contents = "live sibling work must survive\n"
			if err := os.WriteFile(filepath.Join(sibling, "README.md"), []byte(contents), 0o644); err != nil {
				t.Fatal(err)
			}

			if tc.recovered {
				// Let the public lifecycle discover and persist this dirty sibling;
				// no test-authored sibling state is supplied to recovery.
				if _, err := List(poolDir); err != nil {
					t.Fatal(err)
				}
			} else {
				state.Worktrees = append(state.Worktrees, WorktreeEntry{
					Name: siblingName, Path: sibling, CreatedAt: time.Now(), SeedInventoryKnown: true,
				})
				if err := WriteState(poolDir, state); err != nil {
					t.Fatal(err)
				}
				if _, err := LeaseExisting(poolDir, siblingName, "sibling-owner"); err != nil {
					t.Fatal(err)
				}
			}
			before := siblingCleanupEntry(t, poolDir, sibling)
			if !before.Leased || before.LeasedAt.IsZero() {
				t.Fatalf("sibling is not durably protected: %#v", before)
			}
			if tc.recovered {
				if before.LeaseHolder != RecoveredLeaseHolder {
					t.Fatalf("sibling was not automatically recovered: %#v", before)
				}
			} else if before.LeaseID == "" || before.LeaseHolder != "sibling-owner" || before.SeedInventoryDigest == "" {
				t.Fatalf("sibling lease/inventory is not authenticated: %#v", before)
			}

			switch tc.operation {
			case "destroy":
				if _, err := DestroyWorktree(poolDir, target, DestroyOptions{}); err != nil {
					t.Fatal(err)
				}
			case "prune":
				result, err := Prune(repoDir, poolDir, false, nil)
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Pruned) != 1 || result.Pruned[0].Path != target {
					t.Fatalf("pruned = %#v, want only %s", result.Pruned, target)
				}
			case "orphan":
				if err := os.RemoveAll(repoDir); err != nil {
					t.Fatal(err)
				}
				result, err := PruneAllWithOptions(filepath.Dir(poolDir), PruneOptions{PruneOrphans: true})
				if err != nil {
					t.Fatal(err)
				}
				if len(result.Result.Pruned) != 1 || result.Result.Pruned[0].Path != target {
					t.Fatalf("pruned = %#v, want only %s", result.Result.Pruned, target)
				}
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Errorf("target was not removed: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(sibling, "README.md"))
			if err != nil || string(data) != contents {
				t.Errorf("sibling README = %q, %v; want %q", data, err, contents)
			}
			if tc.operation != "orphan" {
				out, err := exec.Command("git", "-C", repoDir, "worktree", "list", "--porcelain", "-z").CombinedOutput()
				if err != nil {
					t.Fatalf("listing Git worktrees: %v\n%s", err, out)
				}
				siblingIdentity, err := canonicalPathPrefix(sibling)
				if err != nil {
					t.Fatal(err)
				}
				targetIdentity, err := canonicalPathPrefix(target)
				if err != nil {
					t.Fatal(err)
				}
				var siblingRegistered, targetRegistered bool
				for _, field := range strings.Split(string(out), "\x00") {
					listedPath, ok := strings.CutPrefix(field, "worktree ")
					if !ok {
						continue
					}
					identity, err := canonicalPathPrefix(filepath.FromSlash(listedPath))
					if err != nil {
						t.Fatalf("resolving Git registration %q: %v", listedPath, err)
					}
					siblingRegistered = siblingRegistered || identity == siblingIdentity
					targetRegistered = targetRegistered || identity == targetIdentity
				}
				if !siblingRegistered {
					t.Errorf("sibling registration missing:\n%s", out)
				}
				if targetRegistered {
					t.Errorf("target registration remains:\n%s", out)
				}
			}
			after := siblingCleanupEntry(t, poolDir, sibling)
			if !after.Leased || after.Path != before.Path || after.LeaseID != before.LeaseID || after.LeaseHolder != before.LeaseHolder || !after.LeasedAt.Equal(before.LeasedAt) {
				t.Errorf("sibling lease changed: before %#v, after %#v", before, after)
			}
			state, err = ReadState(poolDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Worktrees) != 1 {
				t.Errorf("state should retain only sibling: %#v", state.Worktrees)
			}
		})
	}
}

func siblingCleanupEntry(t *testing.T, poolDir, path string) WorktreeEntry {
	t.Helper()
	state, err := ReadState(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range state.Worktrees {
		if entry.Path == path {
			return entry
		}
	}
	t.Fatalf("sibling %s missing from state: %#v", path, state.Worktrees)
	return WorktreeEntry{}
}
