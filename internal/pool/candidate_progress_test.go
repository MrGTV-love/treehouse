package pool

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestCandidateProgressEscapesOnlyLineBreaks(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		path string
		want string
	}{
		{name: "backslash", path: filepath.Join(dir, `copy\one`), want: filepath.Join(dir, `copy\one`)},
		{name: "line feed", path: filepath.Join(dir, "copy\nnext"), want: filepath.Join(dir, `copy\nnext`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			newCandidateProgress(&out)(tc.path, "refused", "dirty")
			want := "treehouse: checked " + tc.want + ": refused: dirty\n"
			if out.String() != want {
				t.Fatalf("progress line = %q, want %q", out.String(), want)
			}
		})
	}
}
