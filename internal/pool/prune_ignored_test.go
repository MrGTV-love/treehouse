package pool

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAcquire_ForeignReclamationPreservesModifiedSeed(t *testing.T) {
	for _, scenario := range []string{"modified", "hardlink-source"} {
		t.Run(scenario, func(t *testing.T) {
			foreign, caller, poolDir := setupSharedClonePool(t)
			for name, content := range map[string]string{
				".gitignore":       "seed.ignored\n",
				".worktreeinclude": "seed.ignored\n",
				"seed.ignored":     "original\n",
			} {
				if err := os.WriteFile(filepath.Join(foreign, name), []byte(content), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			runGit(t, foreign, "add", ".")
			runGit(t, foreign, "commit", "-m", "seed fixture")
			runGit(t, foreign, "push", "origin", "main")
			slot, err := Acquire(foreign, poolDir, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			clearOwnerReservation(t, poolDir, slot)
			file := filepath.Join(slot, "seed.ignored")
			want := "modified\n"
			if scenario == "hardlink-source" {
				want = "original\n"
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(filepath.Join(foreign, "seed.ignored"), file); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(file, []byte(want), 0o640); err != nil {
				t.Fatal(err)
			}
			before, err := ReadState(poolDir)
			if err != nil {
				t.Fatal(err)
			}
			got, err := acquireReclamationCaller(caller, poolDir, 1, false, true)
			if err == nil || got != "" {
				t.Errorf("foreign reclamation deleted a modified seed: %q (%v)", got, err)
			}
			assertFileContents(t, file, want)
			after, err := ReadState(poolDir)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("refusal changed registration: before=%+v after=%+v (%v)", before, after, err)
			}
		})
	}
}

func TestPruneFinalCheckPreservesIgnoredFiles(t *testing.T) {
	for _, scenario := range []string{"unseeded", "modified-seed", "changed-source"} {
		t.Run(scenario, func(t *testing.T) {
			repo, poolDir := setupRepo(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", os.Getenv("HOME"))
			t.Setenv("TREEHOUSE_ROOT", filepath.Dir(poolDir))
			write := func(path, content string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(repo, ".gitignore"), "*.ignored\n")
			if scenario != "unseeded" {
				write(filepath.Join(repo, ".worktreeinclude"), "seed.ignored\n")
				write(filepath.Join(repo, "seed.ignored"), "original\n")
			}
			runGit(t, repo, "add", ".")
			runGit(t, repo, "commit", "-m", "ignored fixture")
			runGit(t, repo, "push", "origin", "main")
			slot, err := Acquire(repo, poolDir, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			clearOwnerReservation(t, poolDir, slot)
			before, err := ReadState(poolDir)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(slot, "seed.ignored")
			target := file
			want := "modified\n"
			if scenario == "unseeded" {
				file = filepath.Join(slot, "unique.ignored")
				target = file
			}
			if scenario == "changed-source" {
				target = filepath.Join(repo, "seed.ignored")
				want = "original\n"
			}
			// This real hook executes after planning AND reservation, immediately
			// before finalPruneSafetyCheck. No production test hook is introduced.
			hook := "echo modified > " + quoteForShell(target)
			result, err := Prune(repo, poolDir, false, []string{hook})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Candidates) != 1 || len(result.Pruned) != 0 || len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason, filepath.Base(file)) {
				t.Errorf("final check did not refuse late payload: %+v", result)
			}
			data, err := os.ReadFile(file)
			if err != nil || strings.TrimSpace(string(data)) != strings.TrimSpace(want) {
				t.Errorf("late file did not survive: %q (%v)", data, err)
			}
			after, err := ReadState(poolDir)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("refusal changed registration or leaked reservation: before=%+v after=%+v (%v)", before, after, err)
			}
			if scenario == "unseeded" {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
			} else {
				write(target, "original\n")
			}
			retry, err := Prune(repo, poolDir, false, nil)
			if err != nil || len(retry.Pruned) != 1 || len(retry.Skipped) != 0 {
				t.Fatalf("resolved condition left stale reservation: %+v (%v)", retry, err)
			}
			if _, err := os.Stat(slot); !os.IsNotExist(err) {
				t.Fatalf("retry did not remove slot: %v", err)
			}
		})
	}
}
