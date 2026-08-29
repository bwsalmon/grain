package main

// cmd/graind (this file's original home, before bwsalmon/agents#313
// folded it into cmd/grain as the "daemon" subcommand) had no tests at
// all before bwsalmon/agents#265 -- every other binary here (cmd/
// mcpserver, cmd/ui, now mcpserver.go and ui.go for the same reason) was
// the same "no test files" gap, but graind was the one that issue asked
// about: the process that wires a real embedded Dolt store, a real
// in-process git proxy, a real Gemini client and the capability registry
// together and runs them on a timer. These tests cover the small pieces
// of that wiring run() itself does not delegate to an already-tested
// package -- readTrimmedFile, credentialTokenSource,
// capabilityProviders, openStore and startGitProxy -- against real
// temp-directory material, the same "real embedded Dolt, not a fake"
// discipline pkg/model/dolt's own tests hold to.
// daemon_live_test.go covers run() itself, end to end, against a real
// Gemini API key.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/v2/pkg/gitproxy"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/dolt"
)

func TestReadTrimmedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("  secret-value\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTrimmedFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret-value" {
		t.Fatalf("readTrimmedFile = %q, want %q", got, "secret-value")
	}
}

func TestReadTrimmedFileMissing(t *testing.T) {
	if _, err := readTrimmedFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error reading a file that does not exist")
	}
}

func TestCredentialTokenSourceTokenFor(t *testing.T) {
	writeCredentials := func(t *testing.T, dir string, patterns map[string]string, tokens map[string]string) *gitproxy.CredentialSet {
		t.Helper()
		data, err := json.Marshal(patterns)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "credentials.json"), data, 0o600); err != nil {
			t.Fatal(err)
		}
		for name, token := range tokens {
			if err := os.WriteFile(filepath.Join(dir, name+".token"), []byte(token), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		set, err := gitproxy.LoadCredentialSet(dir)
		if err != nil {
			t.Fatal(err)
		}
		return set
	}

	t.Run("no ladder configured at all", func(t *testing.T) {
		set, err := gitproxy.LoadCredentialSet(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		src := credentialTokenSource{set}
		if tok := src.TokenFor("acme", "widgets"); tok != nil {
			t.Fatalf("TokenFor with no ladder = %v, want nil", tok)
		}
	})

	t.Run("anonymous credential resolves but carries no token", func(t *testing.T) {
		set := writeCredentials(t, t.TempDir(), map[string]string{"*": "anonymous"}, nil)
		src := credentialTokenSource{set}
		if tok := src.TokenFor("acme", "widgets"); tok != nil {
			t.Fatalf("TokenFor for an anonymous credential = %v, want nil", tok)
		}
	})

	t.Run("a named credential resolves its token file", func(t *testing.T) {
		set := writeCredentials(t, t.TempDir(), map[string]string{"acme/*": "bot"}, map[string]string{"bot": "sekret\n"})
		src := credentialTokenSource{set}
		tok := src.TokenFor("acme", "widgets")
		if tok == nil || *tok != "sekret" {
			t.Fatalf("TokenFor(acme, widgets) = %v, want \"sekret\"", tok)
		}
		if tok := src.TokenFor("other", "repo"); tok != nil {
			t.Fatalf("TokenFor for an unmatched repo = %v, want nil", tok)
		}
	})
}

func TestCapabilityProviders(t *testing.T) {
	cases := []struct {
		name string
		cfg  config
		want []string
	}{
		{
			name: "no gcp project configured still registers github-sandbox",
			cfg:  config{},
			want: []string{"github-sandbox"},
		},
		{
			name: "a gcp project with no agent service account only mints gemini-key, plus github-sandbox",
			cfg:  config{gcpProject: "proj"},
			want: []string{"gemini-key", "github-sandbox"},
		},
		{
			name: "a gcp project with an agent service account mints both, plus github-sandbox",
			cfg:  config{gcpProject: "proj", gcpServiceAccountEmail: "agent@proj.iam.gserviceaccount.com"},
			want: []string{"gcp-key", "gemini-key", "github-sandbox"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providers := capabilityProviders(tc.cfg)
			var got []string
			for _, p := range providers {
				got = append(got, p.Spec().Name)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("capabilityProviders(%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}

func TestOpenStore(t *testing.T) {
	store, db, err := openStore(t.TempDir(), dolt.ServerConfig{})
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer db.Close()
	if store == nil {
		t.Fatal("openStore returned a nil store alongside a nil error")
	}
	// A fresh store has applied its schema, not merely opened a
	// connection: any read against it -- Ready is the one loop.Cycle
	// itself calls every tick -- must succeed rather than error on a
	// missing table.
	if _, err := store.Ready(context.Background()); err != nil {
		t.Fatalf("Ready against a freshly opened store: %v", err)
	}
}

// TestOpenStorePersistsAcrossReopen simulates a graind restart: the same
// -data-dir opened a second time, after the first connection closed,
// must still see what the first one wrote. v2/README.md's "Two things
// the port corrected" flags embedded Dolt's own "database not found on
// a fresh directory" trap; this is the same openStore call exercised a
// second time against a directory that already has a database, which is
// the shape every real restart takes and the fresh-directory case alone
// does not cover.
func TestOpenStorePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	store1, db1, err := openStore(dir, dolt.ServerConfig{})
	if err != nil {
		t.Fatalf("openStore (first): %v", err)
	}
	task := model.Task{
		ID:      "restart-check",
		Intent:  model.IntentImplement,
		Origin:  model.Origin{Attribution: model.Attribution{Actor: model.Principal{Kind: model.PrincipalHuman, ID: "tester"}}, Reason: model.ReasonDirect},
		Binding: model.BindingDirective,
		Target:  &model.RepoRef{Owner: "acme", Name: "widgets"},
	}
	if err := store1.PutTask(context.Background(), task); err != nil {
		t.Fatalf("PutTask: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("closing the first connection: %v", err)
	}

	store2, db2, err := openStore(dir, dolt.ServerConfig{})
	if err != nil {
		t.Fatalf("openStore (second, same dir): %v", err)
	}
	defer db2.Close()
	got, err := store2.GetTask(context.Background(), "restart-check")
	if err != nil {
		t.Fatalf("GetTask after reopen: %v", err)
	}
	if got == nil {
		t.Fatal("task written before the restart did not survive reopening the same -data-dir")
	}
}

// TestLoadConfigSeedsAFreshStoreFromFlags is the first-run case: nothing
// has written grain_config yet, so loadConfig both returns the flags
// untouched and writes them as the seed a UI or a CLI would read next.
func TestLoadConfigSeedsAFreshStoreFromFlags(t *testing.T) {
	store, db, err := openStore(t.TempDir(), dolt.ServerConfig{})
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	flagCfg := config{
		slots: []string{"a", "b"}, pollInterval: time.Minute,
		geminiModel: "gemini-2.5-pro", maxAgentTurns: 10,
		githubHost: "github.example.com", githubInsecureHTTP: true,
		gcpProject: "proj", gcpServiceAccountEmail: "agent@proj.iam.gserviceaccount.com",
	}
	got, err := loadConfig(ctx, store, flagCfg)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !reflect.DeepEqual(got, flagCfg) {
		t.Fatalf("loadConfig on a fresh store = %+v, want the flags unchanged: %+v", got, flagCfg)
	}

	stored, err := store.GetConfig(ctx)
	if err != nil || stored == nil {
		t.Fatalf("GetConfig after seeding: (%+v, %v)", stored, err)
	}
	if !reflect.DeepEqual(*stored, flagCfg.toModelConfig()) {
		t.Fatalf("seeded config = %+v, want %+v", *stored, flagCfg.toModelConfig())
	}
}

// TestLoadConfigPrefersTheStoreOverFlagsOnceOneExists is the restart
// case: a UI or a CLI wrote a config through the store, and the next
// start must run with that rather than whatever the flags on this
// particular invocation happen to say.
func TestLoadConfigPrefersTheStoreOverFlagsOnceOneExists(t *testing.T) {
	store, db, err := openStore(t.TempDir(), dolt.ServerConfig{})
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	stored := model.Config{
		Slots: []string{"only-one"}, PollInterval: 5 * time.Second,
		GeminiModel: "gemini-2.5-flash", MaxAgentTurns: 99,
		GitHubHost: "github.com", GitHubInsecureHTTP: false,
		GCPProject: "stored-proj", GCPServiceAccountEmail: "stored@stored-proj.iam.gserviceaccount.com",
	}
	if err := store.PutConfig(ctx, stored); err != nil {
		t.Fatalf("PutConfig: %v", err)
	}

	flagCfg := config{
		dataDir: "/should/be/left/alone",
		slots:   []string{"whatever", "the", "flags", "say"}, pollInterval: time.Hour,
		geminiModel: "ignored", maxAgentTurns: -1,
		githubHost: "ignored.example.com", githubInsecureHTTP: true,
		gcpProject: "ignored-proj", gcpServiceAccountEmail: "ignored@example.com",
	}
	got, err := loadConfig(ctx, store, flagCfg)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !reflect.DeepEqual(got, flagCfg.withModelConfig(stored)) {
		t.Fatalf("loadConfig with a stored config = %+v, want the stored fields applied: %+v",
			got, flagCfg.withModelConfig(stored))
	}
	// -data-dir is not a grain_config field at all -- loadConfig must
	// leave it exactly as the flags set it, on both branches.
	if got.dataDir != flagCfg.dataDir {
		t.Fatalf("loadConfig changed dataDir to %q, want it left alone", got.dataDir)
	}
}

func TestStartGitProxyServesAndStops(t *testing.T) {
	dataDir := t.TempDir()
	store, db, err := openStore(dataDir, dolt.ServerConfig{})
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer db.Close()

	url, stop, err := startGitProxy(dataDir, store, "example.com", false)
	if err != nil {
		t.Fatalf("startGitProxy: %v", err)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("startGitProxy URL = %q, want an http://127.0.0.1:<port> loopback address", url)
	}

	resp, err := http.Get(url + "/owner/repo.git/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("the git proxy did not answer at all: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if err := stop(context.Background()); err != nil {
		t.Fatalf("stopping the git proxy: %v", err)
	}
	if _, err := http.Get(url); err == nil {
		t.Fatal("expected the git proxy to stop accepting connections once stopped")
	}
}
