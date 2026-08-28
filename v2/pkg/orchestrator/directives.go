package orchestrator

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/model"
)

// directiveRe matches one `/name value` line, optionally indented, with
// nothing after the value — grain/automation/directives.py's own
// _DIRECTIVE_RE, narrowed to the subset this package parses today. `/pr`,
// `/review` and `/depends` are v1 directives this package does not
// support yet: continuing an existing PR and posting a review both need a
// dispatch shape (which branch to check out, review vs. implement intent)
// this package's own RunDispatch does not build yet either, so parsing
// them here would just be dead data. Follow-on work, tracked alongside
// RunDispatch's own single-Intent limitation.
var directiveRe = regexp.MustCompile(`^\s*/(repo|base|auto-merge)\s+(\S+)\s*$`)

// Directives is the subset of grain/automation/directives.py's parsed
// fields this package reads out of a task issue's body.
type Directives struct {
	Repo      *model.RepoRef
	Base      string
	AutoMerge bool
}

// ParseDirectives reads /repo, /base and /auto-merge lines out of body.
// Later lines override earlier ones — matching v1's own "a maintainer can
// repair a task by replying with a corrected directive" rule, applied
// here to a single body rather than body-plus-trusted-replies, since this
// package does not read issue comments at all yet (see poll.go).
func ParseDirectives(body string) (Directives, error) {
	var d Directives
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
				return Directives{}, fmt.Errorf("/repo directive: %w", err)
			}
			d.Repo = &repo
		case "base":
			d.Base = value
		case "auto-merge":
			switch value {
			case "true":
				d.AutoMerge = true
			case "false":
				d.AutoMerge = false
			default:
				return Directives{}, fmt.Errorf("/auto-merge directive: want true or false, got %q", value)
			}
		}
	}
	return d, nil
}
