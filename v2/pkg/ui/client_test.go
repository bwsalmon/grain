package ui

import (
	"errors"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

func testClient() (*Client, *memClient) {
	client := newMemClient()
	cfg := Config{
		TaskRepo:     model.RepoRef{Owner: "acme", Name: "tasks"},
		Labels:       DefaultLabels(),
		Capabilities: DefaultCapabilities(),
	}
	return NewClient(cfg, client), client
}

func TestUpdateTaskChangesOnlyTheFieldsGiven(t *testing.T) {
	c, gh := testClient()
	l := DefaultLabels()
	gh.seed(1, "original title", "original body\n\n/repo acme/widget\n/base main", l.Trigger)

	newTitle := "updated title"
	task, err := c.UpdateTask(1, UpdateTaskRequest{Title: &newTitle})
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "updated title" {
		t.Errorf("title = %q, want %q", task.Title, "updated title")
	}
	if task.Description != "original body" {
		t.Errorf("description changed unexpectedly: %q", task.Description)
	}
	if task.Repo != "acme/widget" || task.Base != "main" {
		t.Errorf("directives changed unexpectedly: repo=%q base=%q", task.Repo, task.Base)
	}
}

func TestUpdateTaskEditsDeclaredFields(t *testing.T) {
	c, gh := testClient()
	l := DefaultLabels()
	gh.seed(2, "title", "body\n\n/repo acme/widget\n/base main", l.Trigger)

	newBase := "release"
	task, err := c.UpdateTask(2, UpdateTaskRequest{Base: &newBase})
	if err != nil {
		t.Fatal(err)
	}
	if task.Base != "release" {
		t.Errorf("base = %q, want release", task.Base)
	}
	if task.Repo != "acme/widget" {
		t.Errorf("repo changed unexpectedly: %q", task.Repo)
	}
}

func TestUpdateTaskClearsRepoOnEmptyString(t *testing.T) {
	c, gh := testClient()
	l := DefaultLabels()
	gh.seed(3, "title", "body\n\n/repo acme/widget", l.Trigger)

	empty := ""
	task, err := c.UpdateTask(3, UpdateTaskRequest{Repo: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if task.Repo != "" {
		t.Errorf("repo = %q, want cleared", task.Repo)
	}
}

func TestUpdateTaskRejectsBadRepo(t *testing.T) {
	c, gh := testClient()
	l := DefaultLabels()
	gh.seed(4, "title", "body", l.Trigger)

	bad := "not-a-repo"
	_, err := c.UpdateTask(4, UpdateTaskRequest{Repo: &bad})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a ValidationError, got %v", err)
	}
}

func TestUpdateTaskRejectsEmptyTitle(t *testing.T) {
	c, gh := testClient()
	l := DefaultLabels()
	gh.seed(5, "title", "body", l.Trigger)

	empty := ""
	_, err := c.UpdateTask(5, UpdateTaskRequest{Title: &empty})
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a ValidationError, got %v", err)
	}
}

func TestUpdateTaskNotFound(t *testing.T) {
	c, _ := testClient()
	_, err := c.UpdateTask(999, UpdateTaskRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}
}
