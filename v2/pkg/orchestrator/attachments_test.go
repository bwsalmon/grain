package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/mcp"
	"github.com/bwsalmon/grain/v2/pkg/model"
)

var attachmentsTestTime = time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

func TestPlaceAttachmentsWritesEveryFileAndNamesItInThePrompt(t *testing.T) {
	root := t.TempDir()
	tools := mcp.NewSandboxTools(root)
	commentID := int64(7)
	attachments := []model.Attachment{
		{ID: 1, Filename: "repro.zip", Content: []byte("PK\x03\x04fake"), CreatedAt: attachmentsTestTime},
		{ID: 2, CommentID: &commentID, Filename: "screenshot.png", Content: []byte("fake png"), CreatedAt: attachmentsTestTime},
	}

	section, err := placeAttachments(context.Background(), tools, attachments)
	if err != nil {
		t.Fatalf("placeAttachments: %v", err)
	}

	for _, want := range []string{
		"attachments/1-repro.zip", "from the task description",
		"attachments/2-screenshot.png", "from comment #7",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("prompt section %q does not mention %q", section, want)
		}
	}

	got, err := os.ReadFile(filepath.Join(root, "attachments", "1-repro.zip"))
	if err != nil {
		t.Fatalf("reading placed attachment: %v", err)
	}
	if string(got) != "PK\x03\x04fake" {
		t.Errorf("attachment 1 content = %q", got)
	}
	got, err = os.ReadFile(filepath.Join(root, "attachments", "2-screenshot.png"))
	if err != nil {
		t.Fatalf("reading placed attachment: %v", err)
	}
	if string(got) != "fake png" {
		t.Errorf("attachment 2 content = %q", got)
	}
}

func TestPlaceAttachmentsIsANoOpWithNone(t *testing.T) {
	// nil tools would fail writeFileTool's own lookup -- proving this
	// returns "" without error confirms a task with nothing attached never
	// even looks for the tool, the same short-circuit BuildPrompt's own
	// Reads section takes for a task with none.
	section, err := placeAttachments(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("placeAttachments: %v", err)
	}
	if section != "" {
		t.Errorf("section = %q, want empty", section)
	}
}

func TestPlaceAttachmentsErrorsWithNoWriteFileTool(t *testing.T) {
	_, err := placeAttachments(context.Background(), nil, []model.Attachment{
		{ID: 1, Filename: "repro.zip", Content: []byte("x"), CreatedAt: attachmentsTestTime},
	})
	if err == nil {
		t.Fatal("expected an error with no write_file tool available")
	}
}
