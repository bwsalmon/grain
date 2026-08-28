package ui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// directiveRe matches pkg/orchestrator.ParseDirectives' own subset of
// grain/automation/directives.py's grammar: /repo, /base, /auto-merge.
// Duplicated rather than imported -- pkg/orchestrator pulls in the agent
// and mcp packages transitively, which this package has no other reason
// to depend on, and the two are three lines each, not a design worth
// sharing a package over.
var directiveRe = regexp.MustCompile(`^\s*/(repo|base|auto-merge)\s+(\S+)\s*$`)

// directives is the declared, form-editable half of a task's body -- what
// a create/edit form renders as fields instead of free text.
type directives struct {
	Repo         *model.RepoRef
	Base         string
	AutoMerge    bool
	HasAutoMerge bool
}

// parseDirectives reads directive lines out of body; later lines win,
// matching ParseDirectives' own rule.
func parseDirectives(body string) (directives, error) {
	var d directives
	for _, line := range strings.Split(body, "\n") {
		m := directiveRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name, value := m[1], m[2]
		switch name {
		case "repo":
			repo, err := model.ParseRepo(value)
			if err != nil {
				return directives{}, fmt.Errorf("/repo directive: %w", err)
			}
			d.Repo = &repo
		case "base":
			d.Base = value
		case "auto-merge":
			switch value {
			case "true":
				d.AutoMerge, d.HasAutoMerge = true, true
			case "false":
				d.AutoMerge, d.HasAutoMerge = false, true
			default:
				return directives{}, fmt.Errorf("/auto-merge directive: want true or false, got %q", value)
			}
		}
	}
	return d, nil
}

// bodyOf renders description plus directive lines for repo/base/autoMerge
// into one issue body -- the create-task form's inverse of
// parseDirectives, so a task authored here parses back the same way
// pkg/orchestrator.ParseDirectives reads it. repo/base empty means no
// line at all, not an empty directive value, since ParseDirectives'
// regex requires a non-empty value and a task with no /repo line is the
// documented "not yet targeted, e.g. scratch-repo" case, not an error.
func bodyOf(description string, repo *model.RepoRef, base string, autoMerge *bool) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(description, "\n"))
	b.WriteString("\n")
	if repo != nil {
		fmt.Fprintf(&b, "\n/repo %s", repo.String())
	}
	if base != "" {
		fmt.Fprintf(&b, "\n/base %s", base)
	}
	if autoMerge != nil {
		fmt.Fprintf(&b, "\n/auto-merge %t", *autoMerge)
	}
	return b.String()
}
