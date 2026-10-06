package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeAliasFixture(t *testing.T, poolDir string, entries ...WorktreeEntry) []byte {
	t.Helper()
	key, err := ensureStateKey(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if entries[i].SeedInventoryKnown {
			entries[i].SeedInventoryDigest = seedInventoryDigest(key, entries[i])
		}
	}
	data, err := json.Marshal(State{Version: stateVersion, Worktrees: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFilePath(poolDir), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestReadState_AliasPreservesAuthoritativeRecord(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, freed := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "canonical-first", true: "alias-first"}[reverse], map[bool]string{false: "quarantined", true: "auto-freed"}[freed]}, "/"), func(t *testing.T) {
				base := t.TempDir()
				poolDir := filepath.Join(base, "pool")
				wtPath := makeFakeWorktree(t, poolDir, "1", "repo")
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(poolDir, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				now := time.Unix(1000, 0).UTC()
				canonical := WorktreeEntry{Name: "1", Path: wtPath, CreatedAt: now, OwnerPID: 42, OwnerStartedAt: 43,
					Leased: true, LeaseID: "live-id", LeaseHolder: "live-owner", LeasedAt: now, BaseBranch: "feature",
					SeededPaths: []string{"dependencies/cache"}, SeedInventoryKnown: true, SeedBackend: "git"}
				duplicate := quarantineEntry("1", filepath.Join(alias, "1", "repo"), "")
				if freed {
					releaseEntry(&duplicate)
				}
				entries := []WorktreeEntry{canonical, duplicate}
				if reverse {
					entries[0], entries[1] = entries[1], entries[0]
				}
				writeAliasFixture(t, poolDir, entries...)
				// Include the original signed inventory in the preservation assertion.
				for _, entry := range entries {
					if entry.LeaseID == canonical.LeaseID {
						canonical = entry
					}
				}
				got, err := ReadState(alias)
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Worktrees) != 1 || !reflect.DeepEqual(got.Worktrees[0], canonical) {
					t.Fatalf("alias load changed live record: got %#v, want %#v", got.Worktrees, canonical)
				}
				if err := WriteState(alias, got); err != nil {
					t.Fatal(err)
				}
				reloaded, err := ReadState(poolDir)
				if err != nil || len(reloaded.Worktrees) != 1 || !reflect.DeepEqual(reloaded.Worktrees[0], canonical) {
					t.Fatalf("consolidated inventory did not authenticate: %#v (%v)", reloaded, err)
				}
				found, err := FindByPath(poolDir, duplicate.Path)
				if err != nil || found == nil || !reflect.DeepEqual(*found, canonical) {
					t.Fatalf("alias lookup did not select live record: %#v (%v)", found, err)
				}
			})
		}
	}
}

func TestReadState_AliasOwnershipConflictPreserved(t *testing.T) {
	for _, field := range []string{"lease", "owner", "seed", "base", "destroying", "name"} {
		t.Run(field, func(t *testing.T) {
			base := t.TempDir()
			poolDir := filepath.Join(base, "pool")
			path := makeFakeWorktree(t, poolDir, "1", "repo")
			alias := filepath.Join(base, "alias")
			if err := os.Symlink(poolDir, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			a := WorktreeEntry{Name: "1", Path: path, Leased: true, LeaseID: "first", LeaseHolder: "owner", SeedInventoryKnown: true}
			b := a
			b.Path = filepath.Join(alias, "1", "repo")
			switch field {
			case "lease":
				b.LeaseID = "second"
			case "owner":
				b.OwnerPID, b.OwnerStartedAt = 42, 43
			case "seed":
				b.SeededPaths, b.SeedBackend = []string{"private-work"}, "git"
			case "base":
				b.BaseBranch = "unlanded"
			case "destroying":
				b.Destroying = true
			case "name":
				b.Name = "2"
			}
			original := writeAliasFixture(t, poolDir, a, b)
			if _, err := ReadState(poolDir); err == nil || !strings.Contains(err.Error(), "conflicting alias records") {
				t.Fatalf("ambiguous records were accepted: %v", err)
			}
			// A lifecycle lookup must also refuse before any callback/reset/removal.
			called := false
			err := ValidateReleasePreconditions(poolDir, b.Path, ReleasePreconditions{}, func() error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("ambiguous ownership reached mutation callback: called=%t err=%v", called, err)
			}
			persisted, err := os.ReadFile(stateFilePath(poolDir))
			if err != nil || string(persisted) != string(original) {
				t.Fatalf("ambiguous ownership was rewritten: %v", err)
			}
		})
	}
}

func TestReadState_DistinctDirectoriesRemainDistinct(t *testing.T) {
	poolDir := t.TempDir()
	a := makeFakeWorktree(t, poolDir, "1", "Repo")
	b := makeFakeWorktree(t, poolDir, "2", "repo")
	writeAliasFixture(t, poolDir,
		WorktreeEntry{Name: "1", Path: a, Leased: true, LeaseID: "one", SeedInventoryKnown: true},
		WorktreeEntry{Name: "2", Path: b, Leased: true, LeaseID: "two", SeedInventoryKnown: true})
	got, err := ReadState(poolDir)
	if err != nil || len(got.Worktrees) != 2 || got.Worktrees[0].LeaseID != "one" || got.Worktrees[1].LeaseID != "two" {
		t.Fatalf("distinct slot identities changed: %#v (%v)", got, err)
	}
}

func TestReadState_CaseSensitiveSiblingsRemainDistinct(t *testing.T) {
	poolDir := t.TempDir()
	a := makeFakeWorktree(t, poolDir, "1", "Repo")
	b := filepath.Join(poolDir, "1", "repo")
	if err := os.Mkdir(b, 0755); os.IsExist(err) {
		t.Skip("filesystem is case-insensitive")
	} else if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, ".git"), []byte("gitdir: /missing"), 0644); err != nil {
		t.Fatal(err)
	}
	writeAliasFixture(t, poolDir,
		WorktreeEntry{Name: "1", Path: a, Leased: true, LeaseID: "one", SeedInventoryKnown: true},
		WorktreeEntry{Name: "other", Path: b, Leased: true, LeaseID: "two", SeedInventoryKnown: true})
	got, err := ReadState(poolDir)
	if err != nil || len(got.Worktrees) != 2 || got.Worktrees[0].Path == got.Worktrees[1].Path {
		t.Fatalf("case-sensitive directories were conflated: %#v (%v)", got, err)
	}
}
