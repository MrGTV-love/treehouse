package main_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/updater"
)

// upstreamRepo is where this fork's code came from. It stays in the module
// path, LICENSE, CHANGELOG history, prose attribution and the no-mistakes
// gate, none of which choose which build a user installs. It must never be
// where an installer, or a command a reader pastes, sends them.
const upstreamRepo = "kunchenguid/treehouse"

// stubTag is the latest release the stubbed GitHub API reports to an
// installer under test.
const stubTag = "v9.9.9"

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// assertFetchesOnlyFromReleaseRepo fails unless every URL an installer
// fetched is ReleaseRepo's latest-release lookup (its releases API, or the
// /releases/latest redirect docs/install.sh resolves the tag from) or one of its
// release downloads, and the installer did both.
func assertFetchesOnlyFromReleaseRepo(t *testing.T, installer, fetchLog string) {
	t.Helper()
	api := "https://api.github.com/repos/" + updater.ReleaseRepo + "/releases/"
	latest := "https://github.com/" + updater.ReleaseRepo + "/releases/latest"
	download := "https://github.com/" + updater.ReleaseRepo + "/releases/download/" + stubTag + "/"
	var sawAPI, sawDownload bool
	for _, url := range strings.Fields(readRepoFile(t, fetchLog)) {
		switch {
		case strings.HasPrefix(url, api), url == latest:
			sawAPI = true
		case strings.HasPrefix(url, download):
			sawDownload = true
		default:
			t.Errorf("%s fetched %s, outside %s's releases", installer, url, updater.ReleaseRepo)
		}
	}
	if !sawAPI || !sawDownload {
		t.Errorf("%s did not both look up the latest release (%v) and download it (%v) from %s", installer, sawAPI, sawDownload, updater.ReleaseRepo)
	}
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestInstallShFetchesOnlyFromReleaseRepo runs docs/install.sh with curl, tar
// and uname stubbed so the URLs it really fetches are recorded.
func TestInstallShFetchesOnlyFromReleaseRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docs/install.sh is the macOS and Linux installer")
	}
	script, err := filepath.Abs(filepath.Join("docs", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := t.TempDir()
	fetchLog := filepath.Join(t.TempDir(), "fetched.log")

	writeStub(t, stubs, "uname", `case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; esac`)
	writeStub(t, stubs, "curl", `out=
url=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    http*) url="$1" ;;
  esac
  shift
done
echo "$url" >> "$FETCH_LOG"
case "$url" in
  */releases/latest) printf '%s' "${url%/latest}/tag/`+stubTag+`"; exit 0 ;;
esac
if [ -n "$out" ]; then : > "$out"; else echo '  "tag_name": "`+stubTag+`",'; fi`)
	writeStub(t, stubs, "tar", `dir=
while [ $# -gt 0 ]; do
  if [ "$1" = -C ]; then dir="$2"; shift; fi
  shift
done
: > "$dir/treehouse"`)

	cmd := exec.Command("sh", script)
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PATH="+stubs+string(os.PathListSeparator)+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"TMPDIR="+t.TempDir(),
		"FETCH_LOG="+fetchLog,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docs/install.sh: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(bin, "treehouse")); err != nil {
		t.Fatalf("docs/install.sh did not install treehouse: %v\n%s", err, out)
	}
	assertFetchesOnlyFromReleaseRepo(t, "docs/install.sh", fetchLog)
}

// TestInstallPs1FetchesOnlyFromReleaseRepo runs docs/install.ps1 with its
// network and archive cmdlets stubbed so the URLs it really fetches are
// recorded. It runs only off Windows, where the installer's user-PATH edit is
// a no-op, so the test never changes the machine's PATH.
func TestInstallPs1FetchesOnlyFromReleaseRepo(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("docs/install.ps1 would edit the real user PATH on Windows")
	}
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not installed")
	}
	script, err := filepath.Abs(filepath.Join("docs", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	fetchLog := filepath.Join(t.TempDir(), "fetched.log")

	stubbed := `
function Invoke-RestMethod { param($Uri) Add-Content -LiteralPath $env:FETCH_LOG -Value $Uri; [pscustomobject]@{ tag_name = '` + stubTag + `' } }
function Invoke-WebRequest { param($Uri, $OutFile) Add-Content -LiteralPath $env:FETCH_LOG -Value $Uri; New-Item -ItemType File -Path $OutFile -Force | Out-Null }
function Expand-Archive { param($Path, $DestinationPath, [switch]$Force) New-Item -ItemType File -Path (Join-Path $DestinationPath 'treehouse.exe') -Force | Out-Null }
. $env:INSTALLER
`
	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-Command", stubbed)
	cmd.Env = append(os.Environ(),
		"LOCALAPPDATA="+t.TempDir(),
		"TMPDIR="+t.TempDir(),
		"FETCH_LOG="+fetchLog,
		"INSTALLER="+script,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docs/install.ps1: %v\n%s", err, out)
	}
	assertFetchesOnlyFromReleaseRepo(t, "docs/install.ps1", fetchLog)
}

// fencedCodeLines returns the lines inside the Markdown file's fenced code
// blocks, keyed by line number: the commands a reader pastes.
func fencedCodeLines(t *testing.T, path string) map[int]string {
	t.Helper()
	lines := map[int]string{}
	inFence := false
	for n, line := range strings.Split(readRepoFile(t, path), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			lines[n+1] = line
		}
	}
	return lines
}

// TestReleaseSourceAgreesEverywhere fails when a command a reader pastes to
// install, run or contribute to a build, or the Nix package's metadata, names
// upstream instead of the fork. Prose may still name upstream as history or
// attribution.
func TestReleaseSourceAgreesEverywhere(t *testing.T) {
	for _, path := range []string{"README.md", "CONTRIBUTING.md"} {
		for n, line := range fencedCodeLines(t, path) {
			if strings.Contains(line, upstreamRepo) {
				t.Errorf("%s:%d names upstream %s: %s", path, n, upstreamRepo, strings.TrimSpace(line))
			}
		}
	}
	for _, path := range []string{"package.nix", "flake.nix", "default.nix"} {
		for n, line := range strings.Split(readRepoFile(t, path), "\n") {
			if strings.Contains(line, upstreamRepo) {
				t.Errorf("%s:%d names upstream %s: %s", path, n+1, upstreamRepo, strings.TrimSpace(line))
			}
		}
	}
}
