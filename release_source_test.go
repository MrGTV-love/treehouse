package main_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/updater"
)

// upstreamRepo is where this fork's code came from. It stays in the module
// path, LICENSE, CHANGELOG history and the no-mistakes gate, none of which
// choose which build a user installs. It must never be where an installer or
// the README sends them.
const upstreamRepo = "kunchenguid/treehouse"

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestReleaseSourceAgreesEverywhere ties every place that names where a build
// is fetched from to updater.ReleaseRepo, so a fork build can neither be
// installed from nor updated to an upstream build.
func TestReleaseSourceAgreesEverywhere(t *testing.T) {
	shell := regexp.MustCompile(`(?m)^REPO="([^"]+)"$`).FindStringSubmatch(readRepoFile(t, "docs/install.sh"))
	if shell == nil || shell[1] != updater.ReleaseRepo {
		t.Errorf("docs/install.sh REPO = %v, want %q", shell, updater.ReleaseRepo)
	}

	powershell := regexp.MustCompile(`(?m)^\$repo = "([^"]+)"$`).FindStringSubmatch(readRepoFile(t, "docs/install.ps1"))
	if powershell == nil || powershell[1] != updater.ReleaseRepo {
		t.Errorf("docs/install.ps1 $repo = %v, want %q", powershell, updater.ReleaseRepo)
	}

	// Everything a reader can paste to install, run or contribute to a build,
	// or that names the project's home, must point at the fork.
	for _, path := range []string{"README.md", "CONTRIBUTING.md", "package.nix", "flake.nix", "default.nix"} {
		for n, line := range strings.Split(readRepoFile(t, path), "\n") {
			if !strings.Contains(line, upstreamRepo) {
				continue
			}
			// The Go module path is import identity: `go install` of it builds
			// upstream, which the README says in so many words.
			if strings.Contains(line, "go install "+"github.com/"+upstreamRepo+"/v3") && strings.Contains(line, "would build upstream") {
				continue
			}
			t.Errorf("%s:%d names upstream %s: %s", path, n+1, upstreamRepo, strings.TrimSpace(line))
		}
	}
}
