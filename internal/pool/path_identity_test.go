package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/vcs"
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

func registeredAliasFixture(t *testing.T) (poolDir, diskPath, aliasPath, backlinkPath string) {
	t.Helper()
	repoDir, poolDir := setupRepo(t)
	diskPath = filepath.Join(poolDir, "1", "repo")
	runGit(t, repoDir, "worktree", "add", "--detach", diskPath, "main")
	alias := filepath.Join(filepath.Dir(poolDir), "alias")
	if err := os.Symlink(poolDir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	aliasPath = filepath.Join(alias, "1", "repo")
	marker, err := os.ReadFile(filepath.Join(diskPath, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	adminDir, ok := strings.CutPrefix(strings.TrimSuffix(string(marker), "\n"), "gitdir: ")
	if !ok || !filepath.IsAbs(adminDir) {
		t.Fatalf("unexpected git marker %q", marker)
	}
	return poolDir, diskPath, aliasPath, filepath.Join(adminDir, "gitdir")
}

func TestReadState_IdleAliasesPreserveRegisteredRecord(t *testing.T) {
	for _, originalIsAlias := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			for _, equalMetadata := range []bool{false, true} {
				name := strings.Join([]string{
					map[bool]string{false: "registered-disk", true: "registered-alias"}[originalIsAlias],
					map[bool]string{false: "original-first", true: "duplicate-first"}[reverse],
					map[bool]string{false: "different-created-at", true: "equal-metadata"}[equalMetadata],
				}, "/")
				t.Run(name, func(t *testing.T) {
					poolDir, diskPath, aliasPath, backlinkPath := registeredAliasFixture(t)
					originalPath, duplicatePath := diskPath, aliasPath
					if originalIsAlias {
						originalPath, duplicatePath = aliasPath, diskPath
					}
					if err := os.WriteFile(backlinkPath, []byte(filepath.Join(originalPath, ".git")+"\n"), 0644); err != nil {
						t.Fatal(err)
					}
					proven, err := vcs.RegisteredWorktreePath(duplicatePath)
					if err != nil || proven != originalPath {
						t.Fatalf("registration proof = %q (%v), want %q", proven, err, originalPath)
					}
					original := WorktreeEntry{Name: "1", Path: originalPath, CreatedAt: time.Unix(1000, 0).UTC(), SeedInventoryKnown: true}
					duplicate := original
					duplicate.Path = duplicatePath
					if !equalMetadata {
						duplicate.CreatedAt = time.Unix(10, 0).UTC()
					}
					entries := []WorktreeEntry{original, duplicate}
					if reverse {
						entries[0], entries[1] = entries[1], entries[0]
					}
					writeAliasFixture(t, poolDir, entries...)
					for _, entry := range entries {
						if entry.Path == originalPath {
							original = entry
						}
					}
					got, err := ReadState(filepath.Dir(filepath.Dir(aliasPath)))
					if err != nil || len(got.Worktrees) != 1 || !reflect.DeepEqual(got.Worktrees[0], original) {
						t.Fatalf("registered record changed: %#v (%v), want %#v", got.Worktrees, err, original)
					}
					if err := WriteState(poolDir, got); err != nil {
						t.Fatal(err)
					}
					reloaded, err := ReadState(poolDir)
					if err != nil || len(reloaded.Worktrees) != 1 || !reflect.DeepEqual(reloaded.Worktrees[0], original) {
						t.Fatalf("reloaded record changed: %#v (%v), want %#v", reloaded.Worktrees, err, original)
					}
					for _, path := range []string{originalPath, duplicatePath} {
						found, err := FindByPath(poolDir, path)
						if err != nil || found == nil || !reflect.DeepEqual(*found, original) {
							t.Fatalf("lookup %s changed record: %#v (%v)", path, found, err)
						}
					}
				})
			}
		}
	}
}

func assertAliasConflictPreserved(t *testing.T, poolDir string, entries ...WorktreeEntry) {
	t.Helper()
	original := writeAliasFixture(t, poolDir, entries...)
	key, err := os.ReadFile(stateKeyPath(poolDir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(poolDir); err == nil || !strings.Contains(err.Error(), "conflicting alias records") {
		t.Fatalf("ambiguous read was accepted: %v", err)
	}
	if err := WriteState(poolDir, State{Version: stateVersion, Worktrees: entries}); err == nil || !strings.Contains(err.Error(), "conflicting alias records") {
		t.Fatalf("ambiguous write was accepted: %v", err)
	}
	called := false
	err = ValidateReleasePreconditions(poolDir, entries[0].Path, ReleasePreconditions{}, func() error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("ambiguous state reached mutation: called=%t err=%v", called, err)
	}
	persisted, err := os.ReadFile(stateFilePath(poolDir))
	if err != nil || string(persisted) != string(original) {
		t.Fatalf("ambiguous state was rewritten: %v", err)
	}
	persistedKey, err := os.ReadFile(stateKeyPath(poolDir))
	if err != nil || string(persistedKey) != string(key) {
		t.Fatalf("ambiguous state key was rewritten: %v", err)
	}
	if _, err := os.Stat(entries[0].Path); err != nil {
		t.Fatalf("ambiguous worktree was removed: %v", err)
	}
}

func TestReadWriteState_UnprovenAliasRecordsFailClosed(t *testing.T) {
	for _, backend := range []string{"git", "jj"} {
		for _, reverse := range []bool{false, true} {
			t.Run(backend+"/"+map[bool]string{false: "disk-first", true: "alias-first"}[reverse], func(t *testing.T) {
				base := t.TempDir()
				poolDir := filepath.Join(base, "pool")
				var diskPath string
				if backend == "git" {
					diskPath = makeFakeWorktree(t, poolDir, "1", "repo")
				} else {
					diskPath = makeFakeJJWorktree(t, poolDir, "1", "repo")
				}
				alias := filepath.Join(base, "alias")
				if err := os.Symlink(poolDir, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				a := WorktreeEntry{Name: "1", Path: diskPath, CreatedAt: time.Unix(1000, 0).UTC(), SeedInventoryKnown: true}
				b := a
				b.Path = filepath.Join(alias, "1", "repo")
				if reverse {
					a, b = b, a
				}
				assertAliasConflictPreserved(t, poolDir, a, b)
			})
		}
	}
}

func TestReadWriteState_RegistrationDoesNotResolveConflictingRecords(t *testing.T) {
	for _, mode := range []string{"lease", "seed", "unmatched-backlink", "foreign-backlink", "quarantine", "registered-unknown", "registered-unknown-live-alias", "registered-unknown-seeded-alias"} {
		for _, reverse := range []bool{false, true} {
			t.Run(mode+"/"+map[bool]string{false: "disk-first", true: "alias-first"}[reverse], func(t *testing.T) {
				poolDir, diskPath, aliasPath, backlinkPath := registeredAliasFixture(t)
				a := WorktreeEntry{Name: "1", Path: diskPath, CreatedAt: time.Unix(1000, 0).UTC(), SeedInventoryKnown: true}
				b := a
				b.Path = aliasPath
				switch mode {
				case "lease":
					a.Leased, b.Leased = true, true
					a.LeaseID, b.LeaseID = "same-lease", "same-lease"
					a.LeaseHolder, b.LeaseHolder = "owner", "owner"
				case "seed":
					a.SeededPaths, b.SeededPaths = []string{"cache"}, []string{"cache"}
					a.SeedBackend, b.SeedBackend = "git", "git"
				case "unmatched-backlink":
					thirdAlias := filepath.Join(filepath.Dir(poolDir), "third-alias")
					if err := os.Symlink(poolDir, thirdAlias); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(backlinkPath, []byte(filepath.Join(thirdAlias, "1", "repo", ".git")+"\n"), 0644); err != nil {
						t.Fatal(err)
					}
				case "foreign-backlink":
					foreignMarker := filepath.Join(t.TempDir(), ".git")
					if err := os.WriteFile(foreignMarker, []byte("unrelated"), 0644); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(backlinkPath, []byte(foreignMarker+"\n"), 0644); err != nil {
						t.Fatal(err)
					}
				case "quarantine":
					a, b = quarantineEntry("1", diskPath, ""), quarantineEntry("1", aliasPath, "")
				case "registered-unknown":
					a = quarantineEntry("1", diskPath, "")
				case "registered-unknown-live-alias":
					a = quarantineEntry("1", diskPath, "")
					b.Leased, b.LeaseID, b.LeaseHolder = true, "live", "owner"
				case "registered-unknown-seeded-alias":
					a = quarantineEntry("1", diskPath, "")
					b.SeededPaths, b.SeedBackend = []string{"cache"}, "git"
				}
				if reverse {
					a, b = b, a
				}
				assertAliasConflictPreserved(t, poolDir, a, b)
			})
		}
	}
}

func TestReadWriteState_RealJJIdleAliasesFailClosed(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "original-first", true: "alias-first"}[reverse], func(t *testing.T) {
			origin, poolDir := setupJJCloneIdentity(t)
			repoDir := filepath.Join(filepath.Dir(poolDir), "jjrepo")
			runJJCloneIdentity(t, "git", "clone", "--colocate", origin, repoDir)
			diskPath := filepath.Join(poolDir, "1", "repo")
			if err := os.MkdirAll(filepath.Dir(diskPath), 0755); err != nil {
				t.Fatal(err)
			}
			if err := vcs.AddWorktree(repoDir, diskPath, "main"); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(filepath.Dir(poolDir), "alias")
			if err := os.Symlink(poolDir, alias); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			a := WorktreeEntry{Name: "1", Path: diskPath, CreatedAt: time.Unix(1000, 0).UTC(), SeedInventoryKnown: true}
			b := a
			b.Path = filepath.Join(alias, "1", "repo")
			if reverse {
				a, b = b, a
			}
			assertAliasConflictPreserved(t, poolDir, a, b)
		})
	}
}

func TestReadState_IdleAliasQuarantinePreservesRegisteredRecord(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, freed := range []bool{false, true} {
			t.Run(map[bool]string{false: "original-first", true: "alias-first"}[reverse]+"/"+map[bool]string{false: "quarantined", true: "auto-freed"}[freed], func(t *testing.T) {
				poolDir, diskPath, aliasPath, backlinkPath := registeredAliasFixture(t)
				if err := os.WriteFile(backlinkPath, []byte(filepath.Join(aliasPath, ".git")+"\n"), 0644); err != nil {
					t.Fatal(err)
				}
				original := WorktreeEntry{Name: "1", Path: aliasPath, CreatedAt: time.Unix(1000, 0).UTC(), SeedInventoryKnown: true}
				duplicate := quarantineEntry("1", diskPath, "")
				if freed {
					releaseEntry(&duplicate)
				}
				entries := []WorktreeEntry{original, duplicate}
				originalIndex := 0
				if reverse {
					entries[0], entries[1] = entries[1], entries[0]
					originalIndex = 1
				}
				writeAliasFixture(t, poolDir, entries...)
				original = entries[originalIndex]
				got, err := ReadState(poolDir)
				if err != nil || len(got.Worktrees) != 1 || !reflect.DeepEqual(got.Worktrees[0], original) {
					t.Fatalf("registered idle record changed: %#v (%v), want %#v", got.Worktrees, err, original)
				}
				if err := WriteState(poolDir, got); err != nil {
					t.Fatal(err)
				}
				reloaded, err := ReadState(poolDir)
				if err != nil || len(reloaded.Worktrees) != 1 || !reflect.DeepEqual(reloaded.Worktrees[0], original) {
					t.Fatalf("reloaded idle record changed: %#v (%v), want %#v", reloaded.Worktrees, err, original)
				}
				found, err := FindByPath(poolDir, diskPath)
				if err != nil || found == nil || !reflect.DeepEqual(*found, original) {
					t.Fatalf("alias lookup changed idle record: %#v (%v)", found, err)
				}
			})
		}
	}
}
