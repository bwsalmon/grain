package main

// Which tools `grain mcpserver` serves for the grants its run holds
// (-grant). This is the whole of how a grant whose only effect is a tool
// roster reaches a running agent: a Framework forks this subcommand and
// can hand it nothing but arguments, so a grant the flag does not carry
// is a tool the prompt can name and no run can call.

import (
	"context"
	"flag"
	"slices"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/mcp"
)

// toolNames is what grantedTools would put on a run's roster.
func toolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

// The bootstrap-playbooks grant needs no source checkout: the playbooks
// are embedded in this binary, so the grant's name is the only thing
// that has to reach this process.
func TestGrantedToolsServesThePlaybooksForTheBootstrapGrant(t *testing.T) {
	names := toolNames(grantedTools(grantNames{"bootstrap-playbooks"}, ""))
	for _, want := range []string{"list_bootstrap_playbooks", "read_bootstrap_playbook"} {
		if !slices.Contains(names, want) {
			t.Errorf("grantedTools = %v, want %s among them", names, want)
		}
	}
	if slices.Contains(names, "read_grain_source") {
		t.Errorf("grantedTools = %v, want nothing of self-debug's for a task without that grant", names)
	}
}

// A task holding both grants at once gets both sets.
func TestGrantedToolsServesEveryGrantsTools(t *testing.T) {
	names := toolNames(grantedTools(grantNames{"self-debug", "bootstrap-playbooks"}, "/src"))
	for _, want := range []string{
		"read_grain_source", "list_grain_source",
		"list_bootstrap_playbooks", "read_bootstrap_playbook",
	} {
		if !slices.Contains(names, want) {
			t.Errorf("grantedTools = %v, want %s among them", names, want)
		}
	}
}

// An ordinary run's roster is exactly the sandbox tools and the escape
// hatches, as it was before any of this existed.
func TestGrantedToolsServesNothingWithoutAGrant(t *testing.T) {
	if tools := grantedTools(nil, "/src"); len(tools) != 0 {
		t.Errorf("grantedTools = %v, want none for a run holding no grant", toolNames(tools))
	}
}

// A playbook really is readable through the tool the grant hands over --
// the end of the road that starts at a human attaching the label.
func TestTheBootstrapGrantsToolsReadAnEmbeddedPlaybook(t *testing.T) {
	tools := grantedTools(grantNames{"bootstrap-playbooks"}, "")
	var list, read mcp.Tool
	for _, tool := range tools {
		switch tool.Name {
		case "list_bootstrap_playbooks":
			list = tool
		case "read_bootstrap_playbook":
			read = tool
		}
	}
	if list.Handler == nil || read.Handler == nil {
		t.Fatalf("grantedTools = %v, want both playbook tools", toolNames(tools))
	}
	listed := list.Handler(context.Background(), nil)
	if listed.IsError || strings.TrimSpace(listed.Text) == "" {
		t.Fatalf("list_bootstrap_playbooks = %+v, want the embedded playbook names", listed)
	}
	name := strings.Split(listed.Text, "\n")[0]
	body := read.Handler(context.Background(), map[string]any{"name": name})
	if body.IsError || strings.TrimSpace(body.Text) == "" {
		t.Fatalf("read_bootstrap_playbook(%q) = %+v, want the playbook's own markdown", name, body)
	}
}

// The flag itself: repeated, one name per pair, in the order the
// Framework wrote them (agent.GrantArgs).
func TestTheGrantFlagCollectsEveryName(t *testing.T) {
	var grants grantNames
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Var(&grants, "grant", "")
	if err := fs.Parse([]string{"-grant", "self-debug", "-grant", "bootstrap-playbooks"}); err != nil {
		t.Fatal(err)
	}
	if want := (grantNames{"self-debug", "bootstrap-playbooks"}); !slices.Equal(grants, want) {
		t.Fatalf("-grant collected %v, want %v", grants, want)
	}
	if !grants.has("bootstrap-playbooks") || grants.has("self-repair") {
		t.Errorf("grants.has misread %v", grants)
	}
}
