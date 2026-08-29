package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/version"
)

func TestVersionCmdPrintsVersionString(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w

	versionCmd(nil)

	w.Close()
	os.Stdout = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}

	got := strings.TrimSpace(string(out))
	want := version.String()
	if got != want {
		t.Errorf("grain version printed %q, want %q (pkg/version.String())", got, want)
	}
}
