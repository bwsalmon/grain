package gemini

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveTranscriptDirTailsAPartialTranscript(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "r1"), []byte("still working\n\n> run_command"), 0o644); err != nil {
		t.Fatal(err)
	}

	live := LiveTranscriptDir{Dir: dir}
	text, ok, err := live.Tail("r1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !strings.Contains(text, "still working") {
		t.Errorf("text = %q, want it to contain %q", text, "still working")
	}
}

func TestLiveTranscriptDirReportsNotOKForAMissingRun(t *testing.T) {
	live := LiveTranscriptDir{Dir: t.TempDir()}
	_, ok, err := live.Tail("no-such-run")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("ok = true for a run with no transcript file, want false")
	}
}
