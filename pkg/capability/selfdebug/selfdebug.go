// Package selfdebug is a GRANT model.CapabilityProvider: a task holding
// it gets read-only tools onto grain's own source checkout --
// bwsalmon/agents#540's "the agent should also be able to read the
// grain source code without permission to do debugging" -- with no
// confirmation step of any kind, since nothing this package exposes can
// change anything. ui.DefaultCapabilities' own "self-debug" row has
// named this since before there was anything behind it; this is that
// provider.
//
// It mints nothing and writes no Placement -- Resolve, Materialize,
// PromptSection and Revoke are all model.BaseCapability's defaults --
// because what it grants is not material moved into a sandbox, it is
// tools (SourceTools below) that orchestrator's own dispatch adds
// straight to a run's tool set once it sees this capability granted on
// an Interactive task. See orchestrator/cycle.go's runOne for that
// wiring, and pkg/capability/selfrepair for this capability's
// destructive counterpart, which does need a confirmation step.
package selfdebug

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bwsalmon/grain/pkg/mcp"
	"github.com/bwsalmon/grain/pkg/model"
)

// CapabilityName is the grant name ui.DefaultCapabilities lists as
// "self-debug".
const CapabilityName = "self-debug"

// Provider is the self-debug capability. Every method but Spec is
// model.BaseCapability's default: Resolve always honours (there is no
// credential to withhold), and Materialize/PromptSection/Revoke all do
// nothing, since nothing here mints a lease or places a file.
type Provider struct {
	model.BaseCapability
}

// New returns a self-debug Provider, ready to register.
func New() *Provider { return &Provider{} }

func (p *Provider) Spec() model.CapabilitySpec {
	return model.CapabilitySpec{
		Name:        CapabilityName,
		Label:       "Self debug",
		Description: "Let this task read grain's own source checkout, to help it debug or explain grain's own behavior",
		Source:      model.GrantByLabel,
		Provision:   model.ProvisionGrant,
	}
}

// SourceTools returns the read-only tools a self-debug grant adds to a
// run: read_grain_source (one file) and list_grain_source (one
// directory), both confined to srcDir the same way mcp.NewSandboxTools'
// own tools are confined to a sandbox root. The confinement is
// reimplemented here, not imported, since mcp exports no path helper of
// its own and this package has no other reason to depend on it beyond
// the Tool/Result shapes.
func SourceTools(srcDir string) []mcp.Tool {
	return []mcp.Tool{readSourceTool(srcDir), listSourceTool(srcDir)}
}

// resolveSourcePath maps a tool-supplied path onto one inside srcDir,
// rejecting anything -- absolute or relative-with-.. -- that would land
// outside it. An empty path means srcDir itself, since list_grain_source
// needs a way to ask for the checkout's own root.
func resolveSourcePath(srcDir, p string) (string, error) {
	if p == "" {
		p = "."
	}
	var full string
	if filepath.IsAbs(p) {
		full = filepath.Clean(p)
	} else {
		full = filepath.Clean(filepath.Join(srcDir, p))
	}
	root := filepath.Clean(srcDir)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes grain's source checkout", p)
	}
	return full, nil
}

var readSourceInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"file_path": map[string]any{
			"type":        "string",
			"description": "path to a file, relative to the root of grain's own source checkout",
		},
	},
	"required": []string{"file_path"},
}

func readSourceTool(srcDir string) mcp.Tool {
	return mcp.Tool{
		Name: "read_grain_source",
		Description: "Read a file from grain's own source checkout, read-only. Needs no permission -- use this " +
			"to look at grain's own code while debugging or explaining its behavior.",
		InputSchema: readSourceInputSchema,
		Handler: func(_ context.Context, args map[string]any) mcp.Result {
			fp, _ := args["file_path"].(string)
			full, err := resolveSourcePath(srcDir, fp)
			if err != nil {
				return mcp.Result{Text: err.Error(), IsError: true}
			}
			data, err := os.ReadFile(full)
			if err != nil {
				return mcp.Result{Text: fmt.Sprintf("Error reading %s: %v", fp, err), IsError: true}
			}
			return mcp.Result{Text: string(data)}
		},
	}
}

var listSourceInputSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"properties": map[string]any{
		"dir_path": map[string]any{
			"type":        "string",
			"description": "path to a directory, relative to the root of grain's own source checkout; omit for the root itself",
		},
	},
}

func listSourceTool(srcDir string) mcp.Tool {
	return mcp.Tool{
		Name: "list_grain_source",
		Description: "List a directory in grain's own source checkout, read-only. Needs no permission -- use " +
			"this to find your way around grain's own code while debugging or explaining its behavior.",
		InputSchema: listSourceInputSchema,
		Handler: func(_ context.Context, args map[string]any) mcp.Result {
			dp, _ := args["dir_path"].(string)
			full, err := resolveSourcePath(srcDir, dp)
			if err != nil {
				return mcp.Result{Text: err.Error(), IsError: true}
			}
			entries, err := os.ReadDir(full)
			if err != nil {
				return mcp.Result{Text: fmt.Sprintf("Error listing %s: %v", dp, err), IsError: true}
			}
			var b strings.Builder
			for _, e := range entries {
				name := e.Name()
				if e.IsDir() {
					name += "/"
				}
				b.WriteString(name)
				b.WriteByte('\n')
			}
			return mcp.Result{Text: b.String()}
		},
	}
}
