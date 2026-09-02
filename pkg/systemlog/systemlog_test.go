package systemlog_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bwsalmon/grain/pkg/systemlog"
)

func TestFileTailReturnsWholeFileWhenShorterThanRequested(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := (systemlog.File{Path: path}).Tail(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"one", "two", "three"}
	if len(lines) != len(want) {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("lines = %v, want %v", lines, want)
		}
	}
}

func TestFileTailReturnsOnlyTheLastNLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lines, err := (systemlog.File{Path: path}).Tail(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"three", "four"}
	if len(lines) != len(want) || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("lines = %v, want %v", lines, want)
	}
}

func TestFileTailOfMissingFileIsEmptyNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.log")
	lines, err := (systemlog.File{Path: path}).Tail(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Fatalf("lines = %v, want empty", lines)
	}
}
