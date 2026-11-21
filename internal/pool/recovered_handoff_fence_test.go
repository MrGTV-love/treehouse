package pool

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveredHandoffRefusesFencedGetThroughReset(t *testing.T) {
	for _, stage := range []string{"claimed", "reset"} {
		t.Run(stage, func(t *testing.T) {
			repo, pool, paths := acquisitionFixture(t, 2)
			original := readFixtureFile(t, filepath.Join(paths[0], "README.md"))
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("new reset target\n"), 0644); err != nil {
				t.Fatal(err)
			}
			runGit(t, repo, "add", "README.md")
			runGit(t, repo, "commit", "-m", "advance reset target")
			old := acquisitionStage
			refused := false
			acquisitionStage = func(point, path string) {
				if point != stage || path != paths[0] {
					return
				}
				assertPoolUnlocked(t, pool)
				beforeBytes, err := os.ReadFile(stateFilePath(pool))
				if err != nil {
					t.Fatal(err)
				}
				state, err := ReadState(pool)
				if err != nil {
					t.Fatal(err)
				}
				for i := range state.Worktrees {
					if state.Worktrees[i].Path == path {
						state.Worktrees[i].Leased, state.Worktrees[i].LeaseHolder = true, RecoveredLeaseHolder
					}
				}
				if err := WriteState(pool, state); err != nil {
					t.Fatal(err)
				}
				damagedBytes, err := os.ReadFile(stateFilePath(pool))
				if err != nil {
					t.Fatal(err)
				}
				_, err = HandoffRecoveredLease(pool, path, RecoveredLeaseHolder, "late-task")
				if !errors.Is(err, ErrAcquisitionInProgress) {
					t.Fatalf("handoff took over a fenced get: %v", err)
				}
				after, err := os.ReadFile(stateFilePath(pool))
				if err != nil || !bytes.Equal(damagedBytes, after) {
					t.Fatalf("refused handoff changed state: %v", err)
				}
				data, err := os.ReadFile(filepath.Join(path, "README.md"))
				if err != nil || string(data) != original {
					t.Fatalf("checkout changed: %q %v", data, err)
				}
				if err := os.WriteFile(stateFilePath(pool), beforeBytes, 0644); err != nil {
					t.Fatal(err)
				}
				refused = true
			}
			t.Cleanup(func() { acquisitionStage = old })
			got, err := AcquireWithOptions(repo, pool, 2, nil, AcquireOptions{SkipFetch: true})
			if !refused || err != nil || got != paths[0] {
				t.Fatalf("get after refused handoff: %q %v refused=%t", got, err, refused)
			}
		})
	}
}
