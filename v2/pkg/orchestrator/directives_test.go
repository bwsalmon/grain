package orchestrator_test

import (
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/orchestrator"
)

func TestParseDirectivesReadsRepoBaseAndAutoMerge(t *testing.T) {
	body := "Please fix the thing.\n\n/repo bwsalmon/grain\n/base develop\n/auto-merge true\n"
	d, err := orchestrator.ParseDirectives(body)
	if err != nil {
		t.Fatalf("ParseDirectives: %v", err)
	}
	if d.Repo == nil || *d.Repo != (model.RepoRef{Owner: "bwsalmon", Name: "grain"}) {
		t.Fatalf("Repo = %+v, want bwsalmon/grain", d.Repo)
	}
	if d.Base != "develop" {
		t.Fatalf("Base = %q, want develop", d.Base)
	}
	if !d.AutoMerge {
		t.Fatal("AutoMerge = false, want true")
	}
}

func TestParseDirectivesLaterLineWins(t *testing.T) {
	body := "/repo a/b\n/repo c/d\n"
	d, err := orchestrator.ParseDirectives(body)
	if err != nil {
		t.Fatalf("ParseDirectives: %v", err)
	}
	if *d.Repo != (model.RepoRef{Owner: "c", Name: "d"}) {
		t.Fatalf("Repo = %+v, want c/d", d.Repo)
	}
}

func TestParseDirectivesIgnoresProseSlashLines(t *testing.T) {
	body := "run /usr/bin/foo and see what happens\n"
	d, err := orchestrator.ParseDirectives(body)
	if err != nil {
		t.Fatalf("ParseDirectives: %v", err)
	}
	if d.Repo != nil {
		t.Fatalf("Repo = %+v, want nil", d.Repo)
	}
}

func TestParseDirectivesRejectsBadRepo(t *testing.T) {
	if _, err := orchestrator.ParseDirectives("/repo not-a-repo\n"); err == nil {
		t.Fatal("expected an error for a malformed /repo directive")
	}
}

func TestParseDirectivesRejectsBadAutoMerge(t *testing.T) {
	if _, err := orchestrator.ParseDirectives("/auto-merge sure\n"); err == nil {
		t.Fatal("expected an error for a non-boolean /auto-merge directive")
	}
}
