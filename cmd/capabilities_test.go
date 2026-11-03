package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The capabilities object is an interface other tools probe, so its field
// names, the schema number, and the persistent_recovered_leases value are
// pinned byte for byte. The version value is the build's version string.
func TestCapabilitiesJSONIsPinned(t *testing.T) {
	setReleaseVersion(t, "v9.8.7-test")
	capabilitiesJSON = true
	t.Cleanup(func() { capabilitiesJSON = false })

	out := captureStdout(t, func() {
		if err := capabilitiesCmd.RunE(capabilitiesCmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	const want = `{"schema_version":1,"version":"v9.8.7-test","persistent_recovered_leases":true}` + "\n"
	if out != want {
		t.Fatalf("capabilities --json = %q, want %q", out, want)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(out), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 3 || fields["persistent_recovered_leases"] != true || fields["schema_version"] != float64(capabilitiesSchema) {
		t.Fatalf("unexpected fields: %v", fields)
	}
}

// The command promises to make no update-check call and start no process. A
// stale cache would make PersistentPreRun spawn the background check, and a
// cached newer release would make it print a notice: neither may happen for
// capabilities, while the same setup does both for an ordinary command.
func TestCapabilitiesNeverReachesTheUpdater(t *testing.T) {
	for name, tc := range map[string]struct {
		cmd           *cobra.Command
		wantSpawn     int
		wantNoticeOut bool
	}{
		"control: an ordinary command": {cmd: &cobra.Command{Use: "status"}, wantSpawn: 1, wantNoticeOut: true},
		"capabilities":                 {cmd: capabilitiesCmd},
	} {
		t.Run(name, func(t *testing.T) {
			home := isolateHome(t)
			setNixStore(t, false)
			setReleaseVersion(t, "v3.0.0")
			writeUpdateCache(t, home, "v9.9.9")
			if err := os.Remove(filepath.Join(home, ".treehouse", "update-check.json")); err != nil {
				t.Fatal(err)
			}
			spawned := 0
			prev := spawnBackgroundCheck
			spawnBackgroundCheck = func(string) error { spawned++; return nil }
			t.Cleanup(func() { spawnBackgroundCheck = prev })

			// A stale cache: it does not exist, so IsCacheStale is true.
			captureStderr(t, func() { rootCmd.PersistentPreRun(tc.cmd, nil) })
			if spawned != tc.wantSpawn {
				t.Fatalf("background check spawned %d time(s), want %d", spawned, tc.wantSpawn)
			}

			// A cached newer release: the notice is printed for the control only.
			writeUpdateCache(t, home, "v9.9.9")
			out := captureStderr(t, func() { rootCmd.PersistentPreRun(tc.cmd, nil) })
			if got := strings.Contains(out, "A new version of treehouse is available"); got != tc.wantNoticeOut {
				t.Fatalf("update notice printed = %v, want %v (%q)", got, tc.wantNoticeOut, out)
			}
		})
	}
}

// End to end, from a directory that is no repository and a home that holds
// nothing: it succeeds, prints the object, and leaves the home empty.
func TestCapabilitiesRunsAnywhere(t *testing.T) {
	home := t.TempDir()
	notARepo := t.TempDir()
	stdout, stderr, code := runTreehouseFromDir(t, notARepo, notARepo, home, nil, "capabilities", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var got capabilitiesReport
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON object: %q: %v", stdout, err)
	}
	if got.SchemaVersion != capabilitiesSchema || !got.PersistentRecoveredLeases || got.Version == "" {
		t.Fatalf("capabilities = %+v", got)
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("capabilities wrote under HOME: %v", entries)
	}
	if _, stderr, code = runTreehouseFromDir(t, notARepo, notARepo, home, nil, "capabilities", "extra"); code == 0 {
		t.Fatalf("an extra argument was accepted: %q", stderr)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	prev := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = prev }()
	fn()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
