package cmd

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/treehouse/v3/internal/pool"
)

// poolDirOf runs "pool-dir" from workDir and returns its single stdout line.
func poolDirOf(t *testing.T, repoDir, workDir, homeDir string, extraEnv []string, args ...string) string {
	t.Helper()
	stdout, stderr, code := runTreehouseFromDir(t, repoDir, workDir, homeDir, extraEnv, append([]string{"pool-dir"}, args...)...)
	if code != 0 {
		t.Fatalf("pool-dir failed (code %d): %s", code, stderr)
	}
	if !strings.HasSuffix(stdout, "\n") || strings.Count(stdout, "\n") != 1 {
		t.Fatalf("expected exactly one stdout line, got %q", stdout)
	}
	return strings.TrimSuffix(stdout, "\n")
}

// leasedPoolDir leases a worktree and returns the pool directory it lives in.
// A slot sits at {pool}/{slot}/{repo}, so the pool is two levels up.
func leasedPoolDir(t *testing.T, repoDir, homeDir string, extraEnv []string, args ...string) string {
	t.Helper()
	stdout, stderr, code := runTreehouse(t, repoDir, homeDir, extraEnv, append([]string{"get", "--lease"}, args...)...)
	if code != 0 {
		t.Fatalf("get --lease failed (code %d): %s", code, stderr)
	}
	return filepath.Dir(filepath.Dir(strings.TrimSpace(stdout)))
}

func TestPoolDirMatchesGet(t *testing.T) {
	writeFile := func(t *testing.T, path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name  string
		setup func(t *testing.T, repoDir, homeDir string) (env, args []string)
		// subdir runs both commands from a subdirectory of the repository.
		subdir string
	}{
		{
			name: "default root",
			setup: func(t *testing.T, repoDir, homeDir string) ([]string, []string) {
				return nil, nil
			},
		},
		{
			name: "TREEHOUSE_ROOT",
			setup: func(t *testing.T, repoDir, homeDir string) ([]string, []string) {
				return []string{"TREEHOUSE_ROOT=" + t.TempDir()}, nil
			},
		},
		{
			name: "project treehouse.toml root",
			setup: func(t *testing.T, repoDir, homeDir string) ([]string, []string) {
				root := filepath.ToSlash(t.TempDir())
				writeFile(t, filepath.Join(repoDir, "treehouse.toml"), "root = \""+root+"\"\n")
				gitCmd(t, repoDir, "add", "treehouse.toml")
				gitCmd(t, repoDir, "commit", "-m", "configure root")
				return nil, nil
			},
		},
		{
			name: "user-level config root",
			setup: func(t *testing.T, repoDir, homeDir string) ([]string, []string) {
				root := filepath.ToSlash(t.TempDir())
				writeFile(t, filepath.Join(homeDir, ".config", "treehouse", "config.toml"), "root = \""+root+"\"\n")
				return nil, nil
			},
		},
		{
			name: "--root absolute",
			setup: func(t *testing.T, repoDir, homeDir string) ([]string, []string) {
				return nil, []string{"--root", t.TempDir()}
			},
		},
		{
			name: "--root . in-project",
			setup: func(t *testing.T, repoDir, homeDir string) ([]string, []string) {
				return nil, []string{"--root", "."}
			},
		},
		{
			name:   "--root . from a subdirectory",
			subdir: "sub",
			setup: func(t *testing.T, repoDir, homeDir string) ([]string, []string) {
				return nil, []string{"--root", "."}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoDir, homeDir := setupTestRepo(t)
			env, args := tt.setup(t, repoDir, homeDir)
			workDir := repoDir
			if tt.subdir != "" {
				workDir = filepath.Join(repoDir, tt.subdir)
				if err := os.MkdirAll(workDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			got := poolDirOf(t, repoDir, workDir, homeDir, env, args...)
			if !filepath.IsAbs(got) {
				t.Fatalf("expected an absolute path, got %q", got)
			}

			getArgs := append([]string{"get", "--lease"}, args...)
			stdout, stderr, code := runTreehouseFromDir(t, repoDir, workDir, homeDir, env, getArgs...)
			if code != 0 {
				t.Fatalf("get --lease failed (code %d): %s", code, stderr)
			}
			want := filepath.Dir(filepath.Dir(strings.TrimSpace(stdout)))
			if got != want {
				t.Fatalf("pool-dir printed %q, but get allocated from %q", got, want)
			}
		})
	}
}

func TestPoolDirWithoutOriginRemote(t *testing.T) {
	repoDir, homeDir := setupLocalOnlyRepo(t)

	got := poolDirOf(t, repoDir, repoDir, homeDir, nil)
	want := leasedPoolDir(t, repoDir, homeDir, nil)
	if got != want {
		t.Fatalf("pool-dir printed %q, but get allocated from %q", got, want)
	}
}

// TestPoolDirHasNoSideEffects pins the promise teardown relies on: asking for
// the directory neither creates it nor touches the repository's exclude file.
func TestPoolDirHasNoSideEffects(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	excludeBefore := readExcludeFile(t, repoDir)

	for _, args := range [][]string{nil, {"--root", "."}} {
		got := poolDirOf(t, repoDir, repoDir, homeDir, nil, args...)
		if _, err := os.Stat(got); !os.IsNotExist(err) {
			t.Fatalf("expected pool-dir %v not to create %s, stat err: %v", args, got, err)
		}
	}

	if after := readExcludeFile(t, repoDir); after != excludeBefore {
		t.Fatalf("pool-dir changed .git/info/exclude:\nbefore: %q\nafter:  %q", excludeBefore, after)
	}
	if names := globalPoolEntries(t, homeDir); len(names) != 0 {
		t.Fatalf("pool-dir created global pool entries: %v", names)
	}
}

// TestPoolDirDoesNotWaitOnPoolLock holds the pool lock the way a long get,
// return, or status snapshot does, and requires pool-dir to answer anyway.
func TestPoolDirDoesNotWaitOnPoolLock(t *testing.T) {
	repoDir, homeDir := setupTestRepo(t)
	want := leasedPoolDir(t, repoDir, homeDir, nil)

	err := pool.WithStateLock(want, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, treehouseBin, "pool-dir")
		cmd.Dir = repoDir
		cmd.Env = buildEnv(homeDir)
		out, err := cmd.Output()
		if err != nil {
			t.Errorf("pool-dir blocked or failed while the pool lock was held: %v", err)
			return nil
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Errorf("pool-dir printed %q, want %q", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to hold the pool lock: %v", err)
	}
}

func TestPoolDirOutsideRepositoryFailsWithNoStdout(t *testing.T) {
	_, homeDir := setupTestRepo(t)
	empty := t.TempDir()

	stdout, stderr, code := runTreehouseFromDir(t, empty, empty, homeDir, nil, "pool-dir")
	if code == 0 {
		t.Fatalf("expected a nonzero exit outside a repository, got 0; stdout: %q", stdout)
	}
	if stdout != "" {
		t.Fatalf("expected no stdout outside a repository, got %q", stdout)
	}

	_, statusStderr, statusCode := runTreehouseFromDir(t, empty, empty, homeDir, nil, "status")
	if statusCode == 0 {
		t.Fatal("expected status to fail outside a repository")
	}
	if stderr != statusStderr {
		t.Fatalf("pool-dir error text differs from status:\npool-dir: %q\nstatus:   %q", stderr, statusStderr)
	}
}
