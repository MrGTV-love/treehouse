package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/treehouse/v3/internal/pool"
)

func TestPrintPruneSkippedEscapesReasonControlCharacters(t *testing.T) {
	var out bytes.Buffer
	printPruneSkipped(&out, []pool.PruneSkipped{{
		Name:     "1",
		Path:     "/pool/1/repo",
		Category: "cannot verify worktree",
		Reason:   "worktree has an initialized submodule (mod\x1b[31m\nfake)",
	}}, false)

	got := out.String()
	if strings.Contains(got, "\x1b") {
		t.Fatalf("reason sent a raw escape byte to the terminal: %q", got)
	}
	want := `worktree has an initialized submodule (mod\u001b[31m\nfake)`
	var reasonLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "/pool/1/repo") {
			reasonLine = line
		}
	}
	if !strings.Contains(reasonLine, want) {
		t.Fatalf("reason was not printed escaped on the slot's own line:\n%s", got)
	}
}
