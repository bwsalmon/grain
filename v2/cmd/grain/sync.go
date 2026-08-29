// sync.go is `grain sync`, bwsalmon/agents#358: the command a GitHub
// Action calls, against a live deployment, whenever a config repo's
// checked-in configuration changes -- the reconciliation half of "add
// commands to the grain cli to allow us to bootstrap external
// infrastructure on a new installation, and the ability to run a syncing
// command in a github action when code or configurations change."
//
// It is a mode of its own (main.go's switch, next to daemon and setup),
// not one of runCLI's task-store commands, because it takes its own
// -config flag rather than the positional task verbs runCLI dispatches
// on.
//
// `grain sync -config path.json` reads one file with up to two
// independent, optional sections:
//
//   - "settings" is unmarshaled straight into ui.UpdateSettingsRequest
//     and applied through HTTPClient.UpdateSettings, over the same
//     -server REST API runCLI's own task commands and `grain settings`
//     use -- sync's whole value over running `grain settings` by hand,
//     one flag at a time, is that the settings live as one reviewable
//     file in a config repo instead of a workflow step's own imperative
//     flag list, and UpdateSettingsRequest's pointer fields already give
//     "only the keys present in the file change anything" for free, no
//     extra work here.
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
// section (v2/README.md) for the workflow shape and how it reaches a
// deployment whose store is bound to loopback only.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"google.golang.org/api/option"

	"github.com/bwsalmon/grain/v2/pkg/gcpsetup"
	"github.com/bwsalmon/grain/v2/pkg/ui"
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

// printSettingsDiff prints only what actually changed -- a sync run
// against a config file whose "settings" section already matches the
// store (the common case, once a deployment is settled) should say so
// plainly rather than re-printing every field on every run.
func printSettingsDiff(before, after ui.Settings) {
	type field struct {
		name        string
		beforeValue string
		afterValue  string
	}
	fields := []field{
		{"poll interval", before.PollInterval, after.PollInterval},
		{"slots", fmt.Sprint(before.Slots), fmt.Sprint(after.Slots)},
		{"gemini model", before.GeminiModel, after.GeminiModel},
		{"max agent turns", fmt.Sprint(before.MaxAgentTurns), fmt.Sprint(after.MaxAgentTurns)},
		{"github host", before.GitHubHost, after.GitHubHost},
		{"github insecure http", fmt.Sprint(before.GitHubInsecureHTTP), fmt.Sprint(after.GitHubInsecureHTTP)},
		{"gcp project", before.GCPProject, after.GCPProject},
		{"gcp agent service account", before.GCPServiceAccountEmail, after.GCPServiceAccountEmail},
	}
	changed := false
	for _, f := range fields {
		if f.beforeValue == f.afterValue {
			continue
		}
		if !changed {
			fmt.Println("settings changed:")
			changed = true
		}
		fmt.Printf("  %s: %q -> %q\n", f.name, f.beforeValue, f.afterValue)
	}
	if !changed {
		fmt.Println("settings: already up to date, nothing changed")
	}
}
