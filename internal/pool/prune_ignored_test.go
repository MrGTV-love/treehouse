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
			if len(result.Candidates) != 1 || len(result.Pruned) != 0 || len(result.Skipped) != 1 || result.Skipped[0].Category != PruneSkipIgnoredFiles || !strings.Contains(result.Skipped[0].Detail, filepath.Base(file)) {
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

func TestPruneOrphansRefusesAnyUnverifiablePath(t *testing.T) {
	for _, scenario := range []string{"clean", "info-exclude", "untracked-and-ignored", "late"} {
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
			runGit(t, repo, "rm", "--quiet", "README.md")
			runGit(t, repo, "commit", "--quiet", "-m", "empty tree")
			runGit(t, repo, "push", "--quiet", "origin", "main")
			write(filepath.Join(repo, ".git", "info", "exclude"), ".env.local\n")
			slot, err := Acquire(repo, poolDir, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			clearOwnerReservation(t, poolDir, slot)
			files := map[string]string{}
			var hooks []string
			switch scenario {
			case "info-exclude":
				files[".env.local"] = "unique secret\n"
			case "untracked-and-ignored":
				files[".env.local"] = "unique secret\n"
				files[filepath.Join("notes", "todo.txt")] = "unique notes\n"
			case "late":
				hooks = []string{"echo unique > " + quoteForShell(filepath.Join(slot, ".env.local"))}
			}
			for name, content := range files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(slot, name)), 0o755); err != nil {
					t.Fatal(err)
				}
				write(filepath.Join(slot, name), content)
			}
			if err := os.RemoveAll(repo); err != nil {
				t.Fatal(err)
			}
			before, err := ReadState(poolDir)
			if err != nil {
				t.Fatal(err)
			}

			result, err := PruneAllWithOptions(filepath.Dir(poolDir), PruneOptions{PruneOrphans: true, PreDestroy: hooks})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "clean" {
				if len(result.Result.Pruned) != 1 || !result.Result.Pruned[0].Orphaned {
					t.Fatalf("clean orphan was not pruned: %+v", result.Result)
				}
				if _, err := os.Stat(slot); !os.IsNotExist(err) {
					t.Fatalf("clean orphan slot survived: %v", err)
				}
				return
			}
			if len(result.Result.Pruned) != 0 || len(result.Result.Skipped) != 1 || result.Result.Skipped[0].Category != PruneSkipIgnoredFiles {
				t.Fatalf("orphan holding unverifiable paths was not refused: %+v", result.Result)
			}
			skip := result.Result.Skipped[0]
			if strings.Contains(skip.Reason, "unseeded") || !strings.Contains(skip.Reason, "orphan") {
				t.Errorf("refusal reason is not orphan-specific: %q", skip.Reason)
			}
			if scenario == "late" {
				files[".env.local"] = "unique\n"
			}
			for name, content := range files {
				if !strings.Contains(skip.Detail, filepath.ToSlash(name)) {
					t.Errorf("refusal does not name %s: %+v", name, skip)
				}
				data, err := os.ReadFile(filepath.Join(slot, name))
				if err != nil || strings.TrimSpace(string(data)) != strings.TrimSpace(content) {
					t.Errorf("%s did not survive: %q (%v)", name, data, err)
				}
			}
			after, err := ReadState(poolDir)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Errorf("refusal changed registration or leaked reservation: before=%+v after=%+v (%v)", before, after, err)
			}
			for name := range files {
				if err := os.Remove(filepath.Join(slot, name)); err != nil {
					t.Fatal(err)
				}
			}
			retry, err := PruneAllWithOptions(filepath.Dir(poolDir), PruneOptions{PruneOrphans: true})
			if err != nil || len(retry.Result.Pruned) != 1 || len(retry.Result.Skipped) != 0 {
				t.Fatalf("resolved orphan was not pruned: %+v (%v)", retry.Result, err)
			}
			if _, err := os.Stat(slot); !os.IsNotExist(err) {
				t.Fatalf("retry did not remove orphan slot: %v", err)
			}
		})
	}
}
