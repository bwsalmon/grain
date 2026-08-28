package ui

import (
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

func TestBodyOfAndParseDirectivesRoundTrip(t *testing.T) {
	repo := &model.RepoRef{Owner: "acme", Name: "widgets"}
	autoMerge := true
	body := bodyOf("Fix the frobnicator.", repo, "release", &autoMerge)

	d, err := parseDirectives(body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Repo == nil || *d.Repo != *repo {
		t.Fatalf("Repo = %v, want %v", d.Repo, repo)
	}
	if d.Base != "release" {
		t.Fatalf("Base = %q, want release", d.Base)
	}
	if !d.HasAutoMerge || !d.AutoMerge {
		t.Fatalf("AutoMerge = %v/%v, want true/true", d.HasAutoMerge, d.AutoMerge)
	}

	desc := stripDirectives(body)
	if desc != "Fix the frobnicator." {
		t.Fatalf("stripDirectives = %q, want %q", desc, "Fix the frobnicator.")
	}
}

func TestBodyOfOmitsUnsetFields(t *testing.T) {
	body := bodyOf("Just a description.", nil, "", nil)
	d, err := parseDirectives(body)
	if err != nil {
		t.Fatal(err)
	}
	if d.Repo != nil {
		t.Fatalf("Repo = %v, want nil", d.Repo)
	}
	if d.Base != "" {
		t.Fatalf("Base = %q, want empty", d.Base)
	}
	if d.HasAutoMerge {
		t.Fatalf("HasAutoMerge = true, want false")
	}
}

func TestParseDirectivesRejectsBadRepo(t *testing.T) {
	if _, err := parseDirectives("body\n/repo not-a-repo\n"); err == nil {
		t.Fatal("want an error for a /repo line with no owner/name split")
	}
}
