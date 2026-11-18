package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/pool"
)

// These fixtures intentionally do not return the acquired lease: return resets
// seeded files. The test-only idle transition retains the authenticated seed
// inventory and bytes, as if an acquisition owner exited without returning.
func TestPruneIgnoredFiles(t *testing.T) {
	for _, all := range []bool{false, true} {
		for _, scenario := range []string{
			"unseeded", "modified-same-size", "modified-size", "unchanged",
			"untracked", "mixed-unseeded", "mixed-modified", "source-changed",
			"source-missing", "source-directory", "source-symlink", "slot-symlink",
			"reverted", "unknown-inventory", "invalid-inventory",
			"hardlink-source", "seeded-symlink", "modified-equal-source-time",
			"equal-new-source", "equal-different-time", "mode-only",
		} {
			t.Run(fmt.Sprintf("all=%t/%s", all, scenario), func(t *testing.T) {
				repo, home := setupTestRepo(t)
				root := filepath.Join(home, "scratch-pools")
				env := []string{"TREEHOUSE_ROOT=" + root, "TREEHOUSE_VCS=git"}
				run := func(args ...string) string {
					t.Helper()
					args = append([]string{"--root", root}, args...)
					out, stderr, code := runTreehouse(t, repo, home, env, args...)
					t.Logf("treehouse %v: exit=%d\n%s%s", args, code, out, stderr)
					if code != 0 {
						t.Fatalf("command failed: %s", stderr)
					}
					return out
				}
				write := func(path, content string) {
					t.Helper()
					if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
						t.Fatal(err)
					}
				}
				seeded := scenario != "unseeded" && scenario != "untracked"
				write(filepath.Join(repo, ".gitignore"), "*.ignored\n")
				if seeded {
					write(filepath.Join(repo, ".worktreeinclude"), "seed.ignored\nother.ignored\n")
					write(filepath.Join(repo, "seed.ignored"), "original\n")
					write(filepath.Join(repo, "other.ignored"), "unchanged\n")
					if scenario == "seeded-symlink" {
						if err := os.Remove(filepath.Join(repo, "seed.ignored")); err != nil {
							t.Fatal(err)
						}
						if err := os.Symlink("other.ignored", filepath.Join(repo, "seed.ignored")); err != nil {
							t.Fatal(err)
						}
					}
				}
				gitCmd(t, repo, "add", ".")
				gitCmd(t, repo, "commit", "-m", "prune ignored fixture")
				gitCmd(t, repo, "push", "origin", "main")
				getArgs := []string{"get", "--lease"}
				// Cover explicit manifests too, without changing the selected files.
				if seeded && all {
					getArgs = append(getArgs, "--include-file", filepath.Join(repo, ".worktreeinclude"))
				}
				slot := strings.TrimSpace(run(getArgs...))
				poolDir := filepath.Dir(filepath.Dir(slot))
				state, err := pool.ReadState(poolDir)
				if err != nil || len(state.Worktrees) != 1 {
					t.Fatalf("acquisition state: %#v (%v)", state, err)
				}
				if !state.Worktrees[0].SeedInventoryKnown || (seeded && len(state.Worktrees[0].SeededPaths) != 2) {
					t.Fatalf("fixture was not authentically seeded: %#v", state.Worktrees[0])
				}
				file := filepath.Join(slot, "seed.ignored")
				var seededTime time.Time
				if seeded {
					sourceInfo, err := os.Stat(filepath.Join(repo, "seed.ignored"))
					if err != nil {
						t.Fatal(err)
					}
					slotInfo, err := os.Stat(file)
					if err != nil {
						t.Fatal(err)
					}
					if scenario != "seeded-symlink" && sourceInfo.Mode().Perm() != slotInfo.Mode().Perm() {
						t.Fatal("seeding did not preserve source permission bits")
					}
					sourceBytes, err := os.ReadFile(filepath.Join(repo, "seed.ignored"))
					if err != nil {
						t.Fatal(err)
					}
					if scenario == "seeded-symlink" {
						target, err := os.Readlink(filepath.Join(repo, "seed.ignored"))
						if err != nil {
							t.Fatal(err)
						}
						sourceBytes = []byte(target)
						if !slotInfo.Mode().IsRegular() {
							t.Fatalf("seeding did not materialize link text as a regular file: %v", slotInfo.Mode())
						}
						t.Logf("Fresh real seeding writes regular-file link-target text %q", target)
					}
					slotBytes, err := os.ReadFile(file)
					if err != nil || !bytes.Equal(sourceBytes, slotBytes) {
						t.Fatalf("real get did not copy equal seed bytes: %v", err)
					}
					t.Logf("seed source mode=%v slot mode=%v; copied bytes=%q", sourceInfo.Mode(), slotInfo.Mode(), sourceBytes)
					seededTime = slotInfo.ModTime()
				}
				reason := "seed.ignored"
				switch scenario {
				case "unseeded", "mixed-unseeded":
					file = filepath.Join(slot, "unique.ignored")
					reason = "unique.ignored"
					write(file, "unique ignored payload\n")
				case "modified-same-size", "mixed-modified":
					write(file, "modified\n")
					if err := os.Chtimes(file, seededTime, seededTime); err != nil {
						t.Fatal(err)
					}
				case "hardlink-source":
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
					if err := os.Link(filepath.Join(repo, "seed.ignored"), file); err != nil {
						t.Fatal(err)
					}
					a, err := os.Stat(file)
					if err != nil {
						t.Fatal(err)
					}
					b, err := os.Stat(filepath.Join(repo, "seed.ignored"))
					if err != nil {
						t.Fatal(err)
					}
					if !os.SameFile(a, b) {
						t.Fatal("hard-link fixture is not the same inode")
					}
				case "modified-equal-source-time":
					// Align every seed, so an mtime-only mutant cannot hide the
					// changed bytes behind an unrelated control's timestamp.
					for _, name := range state.Worktrees[0].SeededPaths {
						info, err := os.Stat(filepath.Join(repo, filepath.FromSlash(name)))
						if err != nil {
							t.Fatal(err)
						}
						if err := os.Chtimes(filepath.Join(slot, filepath.FromSlash(name)), info.ModTime(), info.ModTime()); err != nil {
							t.Fatal(err)
						}
					}
					write(file, "modified\n")
					info, err := os.Stat(filepath.Join(repo, "seed.ignored"))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Chtimes(file, info.ModTime(), info.ModTime()); err != nil {
						t.Fatal(err)
					}
					got, err := os.Stat(file)
					if err != nil {
						t.Fatal(err)
					}
					if got.Size() != info.Size() || !got.ModTime().Equal(info.ModTime()) {
						t.Fatal("equal-size equal-time fixture not established")
					}
				case "equal-new-source":
					write(file, "modified\n")
					write(filepath.Join(repo, "seed.ignored"), "modified\n")
				case "equal-different-time":
					if err := os.Chtimes(file, seededTime.Add(-time.Hour), seededTime.Add(-time.Hour)); err != nil {
						t.Fatal(err)
					}
				case "mode-only":
					if err := os.Chmod(file, 0o600); err != nil {
						t.Fatal(err)
					}
				case "modified-size":
					write(file, "longer unique ignored payload\n")
				case "source-changed":
					write(filepath.Join(repo, "seed.ignored"), "modified\n")
				case "source-missing", "source-directory", "source-symlink":
					source := filepath.Join(repo, "seed.ignored")
					if err := os.Remove(source); err != nil {
						t.Fatal(err)
					}
					if scenario == "source-directory" {
						if err := os.Mkdir(source, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "source-symlink" {
						if err := os.Symlink(file, source); err != nil {
							t.Fatal(err)
						}
					}
				case "slot-symlink":
					if err := os.Remove(file); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Join(repo, "seed.ignored"), file); err != nil {
						t.Fatal(err)
					}
				case "reverted":
					write(file, "modified\n")
					write(file, "original\n")
				case "untracked":
					file = filepath.Join(slot, "unique.txt")
					reason = "uncommitted changes"
					write(file, "untracked payload\n")
				}
				if scenario != "untracked" {
					if status := gitCmd(t, slot, "status", "--porcelain", "--untracked-files=all"); status != "" {
						t.Fatalf("fixture is not ignored-only: %s", status)
					}
					gitCmd(t, slot, "check-ignore", "--", filepath.Base(file))
				}
				if gitCmd(t, slot, "rev-parse", "HEAD") != gitCmd(t, repo, "rev-parse", "origin/main") {
					t.Fatal("fixture HEAD is not landed")
				}
				// Only lease/reservation fields change. WriteState reauthenticates
				// the existing inventory without altering its paths or source.
				entry := &state.Worktrees[0]
				entry.Leased, entry.Destroying = false, false
				entry.LeaseID, entry.LeaseHolder = "", ""
				entry.LeasedAt = time.Time{}
				entry.OwnerPID, entry.OwnerStartedAt = 0, 0
				if scenario == "unknown-inventory" {
					entry.SeedInventoryKnown = false
				}
				if err := pool.WriteState(poolDir, state); err != nil {
					t.Fatal(err)
				}
				statePath := filepath.Join(poolDir, "treehouse-state.json")
				if scenario == "invalid-inventory" {
					data, err := os.ReadFile(statePath)
					if err != nil {
						t.Fatal(err)
					}
					var raw map[string]any
					if err := json.Unmarshal(data, &raw); err != nil {
						t.Fatal(err)
					}
					raw["worktrees"].([]any)[0].(map[string]any)["seed_inventory_digest"] = "invalid"
					data, err = json.Marshal(raw)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(statePath, data, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				before := pruneFileInventory(t, slot)
				pruneFileInventory(t, statePath)
				pruneArgs := []string{"prune"}
				if all {
					pruneArgs = append(pruneArgs, "--all")
				}
				dry := run(pruneArgs...)
				out := run(append(pruneArgs, "--yes")...)
				allow := scenario == "unchanged" || scenario == "reverted" || scenario == "seeded-symlink" || scenario == "equal-new-source" || scenario == "equal-different-time" || scenario == "mode-only"
				if allow {
					if _, err := os.Lstat(slot); !os.IsNotExist(err) {
						t.Errorf("regenerable slot not removed: %v", err)
					}
					if !strings.Contains(out, "Pruned 1 stale worktree") {
						t.Errorf("missing successful removal: %s", out)
					}
				} else {
					after := pruneFileInventory(t, slot)
					if !reflect.DeepEqual(before, after) {
						t.Errorf("prune lost or changed payload: before=%v after=%v", before, after)
					}
					quarantine := strings.HasSuffix(scenario, "inventory")
					if !quarantine && (!strings.Contains(dry, reason) || !strings.Contains(out, reason)) {
						t.Errorf("missing refusal %q: dry=%s execute=%s", reason, dry, out)
					}
				}
				afterState, err := pool.ReadState(poolDir)
				if err != nil || len(afterState.Worktrees) != map[bool]int{true: 0, false: 1}[allow] {
					t.Errorf("unexpected registration after prune: %#v (%v)", afterState, err)
				}
				pruneFileInventory(t, statePath)
			})
		}
	}
}

// Return only type, bytes and mode for the no-loss oracle. Log all native stat
// fields too (including ctime on Unix), so operational metadata writes remain
// evidence without confusing them with loss of a user's file.
func pruneFileInventory(t *testing.T, root string) map[string]string {
	t.Helper()
	inventory := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += fmt.Sprintf(":%x", sha256.Sum256(data))
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += ":" + target
		}
		inventory[rel] = value
		record, err := json.Marshal(map[string]any{"path": path, "type_mode_hash": value, "size": info.Size(), "mtime_ns": info.ModTime().UnixNano(), "native_stat": info.Sys()})
		if err != nil {
			return err
		}
		t.Log(string(record))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return inventory
}
