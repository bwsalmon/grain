// sync.go is `grain sync`, bwsalmon/agents#358: the command a GitHub
// Action calls, against a live deployment, whenever a config repo's
// checked-in configuration changes -- the reconciliation half of "add
// commands to the grain cli to allow us to bootstrap external
// infrastructure on a new installation, and the ability to run a syncing
// command in a github action when code or configurations change."
//
// It is a mode of its own (main.go's switch, next to daemon/mcpserver
// and setup), not one of runCLI's task-store commands, mainly so a
// "gcp"-only sync -- one with no "settings" section -- never needs
// -server, or anything else about a running daemon, at all.
//
// `grain sync -config path.json` reads one file with up to two
// independent, optional sections:
//
//   - "settings" is unmarshaled straight into ui.UpdateSettingsRequest
//     and applied through HTTPClient.UpdateSettings, against -server, the
//     same running daemon `grain settings` itself talks to (bwsalmon/
//     agents#363: there is no store flag here to open the database
//     directly with any more) -- sync's whole value over that is that the
//     settings live as one reviewable file in a config repo instead of a
//     workflow step's own imperative flag list, and
//     UpdateSettingsRequest's pointer fields already give "only the keys
//     present in the file change anything" for free, no extra work here.
//   - "gcp" re-runs pkg/gcpsetup.EnsureInfrastructure, the same
//     reconciliation `grain setup gcp` uses for a brand new installation
//     (setup.go's own doc comment) -- run here on every sync instead of
//     once at install time, so IAM drift (a binding removed by hand, a
//     newly-enabled gemini-key capability that needs a grant it didn't
//     before) gets repaired automatically. It never mints a new minter
//     key -- gcpsetup.Options.MintMinterKey's own doc comment explains
//     why that stays a one-time, explicit `grain setup gcp -mint-key`.
//
// Both sections use the exact same "safe to re-run, nothing to do if
// nothing changed" discipline scripts/setup.sh already holds itself to,
// so pointing a workflow at `grain sync` on every push touching the
// config file is the whole of what "run this in a GitHub Action when code
// or configurations change" needs -- see this package's own README
// section (README.md) for the workflow shape and how it reaches a
// deployment whose UI/API is bound to loopback only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"google.golang.org/api/option"

	"github.com/bwsalmon/grain/pkg/gcpsetup"
	"github.com/bwsalmon/grain/pkg/ui"
)

// syncConfig is grain sync's -config file shape -- see this file's own
// doc comment for what each section does.
type syncConfig struct {
	Settings *ui.UpdateSettingsRequest `json:"settings"`
	GCP      *syncGCPConfig            `json:"gcp"`
}

// syncGCPConfig names the same knobs `grain setup gcp` takes as flags,
// as JSON fields instead -- gcpsetup.Options restated for this file's
// shape (see gcpsetup.Options's own doc comment for what each one does).
type syncGCPConfig struct {
	Project         string `json:"project"`
	CredentialsFile string `json:"credentialsFile"`
	AgentAccountID  string `json:"agentAccountId"`
	MinterAccountID string `json:"minterAccountId"`
	EnableGeminiKey bool   `json:"enableGeminiKey"`
}

func syncCmd(args []string) {
	if err := cmdSync(context.Background(), args); err != nil {
		fmt.Fprintln(os.Stderr, "grain sync: "+err.Error())
		os.Exit(1)
	}
}

func cmdSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("grain sync", flag.ExitOnError)
	configPath := fs.String("config", "", "path to a JSON file with \"settings\" and/or \"gcp\" sections to reconcile (required)")
	server := fs.String("server", defaultServerURL, "base URL of a running \"grain daemon\"'s UI/API -- only needed if -config has a \"settings\" section")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}

	data, err := os.ReadFile(*configPath)
	if err != nil {
		return fmt.Errorf("reading -config: %w", err)
	}
	var cfg syncConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parsing -config: %w", err)
	}
	if cfg.Settings == nil && cfg.GCP == nil {
		return errors.New("-config has neither a \"settings\" nor a \"gcp\" section -- nothing to sync")
	}

	if cfg.GCP != nil {
		if err := syncGCP(ctx, *cfg.GCP); err != nil {
			return fmt.Errorf("reconciling GCP infrastructure: %w", err)
		}
	}

	if cfg.Settings != nil {
		if err := syncSettings(ctx, *cfg.Settings, *server); err != nil {
			return fmt.Errorf("applying settings: %w", err)
		}
	}
	return nil
}

func syncGCP(ctx context.Context, gc syncGCPConfig) error {
	if gc.Project == "" {
		return errors.New("gcp.project is required")
	}
	var opts []option.ClientOption
	if gc.CredentialsFile != "" {
		opts = append(opts, option.WithCredentialsFile(gc.CredentialsFile))
	}
	admin, err := gcpsetup.NewRealAdmin(ctx, gc.Project, opts...)
	if err != nil {
		return err
	}
	result, err := gcpsetup.EnsureInfrastructure(ctx, admin, gcpsetup.Options{
		ProjectID:       gc.Project,
		AgentAccountID:  gc.AgentAccountID,
		MinterAccountID: gc.MinterAccountID,
		EnableGeminiKey: gc.EnableGeminiKey,
	})
	if err != nil {
		return err
	}
	printSetupResult(result)
	fmt.Println()
	return nil
}

func syncSettings(ctx context.Context, req ui.UpdateSettingsRequest, server string) error {
	c := ui.NewHTTPClient(server)

	before, err := c.GetSettings(ctx)
	if err != nil {
		return err
	}
	after, err := c.UpdateSettings(ctx, req)
	if err != nil {
		return err
	}
	printSettingsDiff(before, after)
	return nil
}

// settingsDiffRow is one line printSettingsDiff can print: which setting
// a config file changed, what to call it, and how to read it back off the
// settings the daemon reported before and after.
type settingsDiffRow struct {
	// field is the ui.UpdateSettingsRequest JSON name of the setting this
	// row reports -- the very key a config file's "settings" section
	// spells it with. It is what keeps the table honest rather than
	// merely well-intentioned: settingsDiffRowsCoverEveryUpdatableSetting
	// reflects over that struct and fails naming any field no row here
	// claims, so a new setting cannot become syncable without a line of
	// output to go with it.
	field string
	// name is what a workflow log calls the setting -- prose, not the
	// JSON key, since this is read by whoever is looking at the run.
	name string
	// value reads the setting out of one side of the diff. Printed and
	// compared through fmt.Sprint, so a row may return whatever type the
	// setting is (or a string it has composed, as the two list settings
	// below do).
	value func(ui.Settings) any
}

// settingsDiffRows is every setting `grain sync` can change, in the order
// a diff prints them.
//
// Keyed by ui.UpdateSettingsRequest's own JSON names because that struct
// is already the definition of "what a config file can set": syncSettings
// hands UpdateSettings the whole request the file carries, so anything
// with a field there is syncable whether or not anyone remembered this
// table. It was a hand-written list of names for long enough to go
// silently stale twice -- the sandbox VM shape (bwsalmon/agents#534)
// first, then seven more at once (target repos, default capabilities,
// agent framework, newest first, show closed by default, approved by
// default and auto merge by default) -- and the cost each time was the
// worst one this command has: `grain sync` narrowing a deployment's
// target repos, or turning auto-merge on for every task filed after it,
// and printing "already up to date, nothing changed". A sync that changes
// something has to say what.
var settingsDiffRows = []settingsDiffRow{
	{"environmentName", "environment name", func(s ui.Settings) any { return s.EnvironmentName }},
	{"timeZone", "time zone", func(s ui.Settings) any { return s.TimeZone }},
	{"pollInterval", "poll interval", func(s ui.Settings) any { return s.PollInterval }},
	{"maxWorkers", "max workers", func(s ui.Settings) any { return s.MaxWorkers }},
	{"maxMergers", "max mergers", func(s ui.Settings) any { return s.MaxMergers }},
	{"geminiModel", "gemini model", func(s ui.Settings) any { return s.GeminiModel }},
	{"geminiEffort", "gemini effort", func(s ui.Settings) any { return s.GeminiEffort }},
	{"claudeModel", "claude model", func(s ui.Settings) any { return s.ClaudeModel }},
	{"codexModel", "codex model", func(s ui.Settings) any { return s.CodexModel }},
	{"maxAgentTurns", "max agent turns", func(s ui.Settings) any { return s.MaxAgentTurns }},
	{"githubHost", "github host", func(s ui.Settings) any { return s.GitHubHost }},
	{"githubInsecureHttp", "github insecure http", func(s ui.Settings) any { return s.GitHubInsecureHTTP }},
	{"gcpProject", "gcp project", func(s ui.Settings) any { return s.GCPProject }},
	{"gcpServiceAccountEmail", "gcp agent service account", func(s ui.Settings) any { return s.GCPServiceAccountEmail }},
	{"sandboxCpus", "sandbox cpus", func(s ui.Settings) any { return s.SandboxCPUs }},
	{"sandboxMemoryMb", "sandbox memory mb", func(s ui.Settings) any { return s.SandboxMemoryMB }},
	{"sandboxDiskGb", "sandbox disk gb", func(s ui.Settings) any { return s.SandboxDiskGB }},
	// Printed the same %q way as everything else despite being the one
	// setting here that can be several lines long: a diff is where the
	// change is worth seeing whole, and %q keeps a multi-line value on one
	// line of output rather than breaking the two-column shape of every
	// line around it.
	{"promptExtension", "prompt extension", func(s ui.Settings) any { return s.PromptExtension }},
	// The two lists are joined rather than printed as Go slices: this is
	// output a workflow log is read in, and ["a" "b"] is not how anything
	// else here names a set. Joining is also what they are compared on, so
	// two equal lists read as the no change they are.
	{"targetRepos", "target repos", func(s ui.Settings) any { return strings.Join(s.TargetRepos, ", ") }},
	{"defaultCapabilities", "default capabilities", func(s ui.Settings) any { return strings.Join(s.DefaultCapabilities, ", ") }},
	{"agentFramework", "agent framework", func(s ui.Settings) any { return s.AgentFramework }},
	{"newestFirst", "newest first", func(s ui.Settings) any { return s.NewestFirst }},
	{"showClosedByDefault", "show closed by default", func(s ui.Settings) any { return s.ShowClosedByDefault }},
	{"approvedByDefault", "approved by default", func(s ui.Settings) any { return s.ApprovedByDefault }},
	{"autoMergeByDefault", "auto merge by default", func(s ui.Settings) any { return s.AutoMergeByDefault }},
}

// settingsDiffExceptions names any ui.UpdateSettingsRequest field that
// deliberately has no row above, and why -- the one place a gap in the
// table is allowed to be recorded, so that a setting missing from the
// diff is a decision somebody wrote a reason for rather than the
// oversight it has twice been.
//
// Empty today: every setting a config file can set is reported back by
// GetSettings, so every one of them has something to diff. A field that
// genuinely has nothing to compare against -- one UpdateSettings accepts
// but ui.Settings never reports back, say -- belongs here with the reason
// it cannot be diffed. The test rejects a stale entry too, so an
// exception outliving the field it excuses fails rather than quietly
// excusing nothing.
var settingsDiffExceptions = map[string]string{}

// printSettingsDiff prints only what actually changed -- a sync run
// against a config file whose "settings" section already matches the
// store (the common case, once a deployment is settled) should say so
// plainly rather than re-printing every field on every run.
func printSettingsDiff(before, after ui.Settings) {
	changed := false
	for _, row := range settingsDiffRows {
		beforeValue, afterValue := fmt.Sprint(row.value(before)), fmt.Sprint(row.value(after))
		if beforeValue == afterValue {
			continue
		}
		if !changed {
			fmt.Println("settings changed:")
			changed = true
		}
		fmt.Printf("  %s: %q -> %q\n", row.name, beforeValue, afterValue)
	}
	if !changed {
		fmt.Println("settings: already up to date, nothing changed")
	}
}
