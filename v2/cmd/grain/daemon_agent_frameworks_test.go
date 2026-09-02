package main

// Choosing (and crediting) the agent framework a dispatch runs under:
// the deployment's own default read live from the store, a task's
// override of it, and the credential each one needs -- read from the
// secrets database the UI writes before the file a deployment was seeded
// from.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/v2/pkg/agent/claude"
	"github.com/bwsalmon/grain/v2/pkg/model"
	"github.com/bwsalmon/grain/v2/pkg/model/sqlite"
	"github.com/bwsalmon/grain/v2/pkg/secrets"
)

func testSecrets(t *testing.T) *secrets.Store {
	t.Helper()
	return secrets.New(t.TempDir())
}

func testStore(t *testing.T) *model.Store {
	t.Helper()
	db, err := sqlite.Open(sqlite.DefaultConfig(t.TempDir()))
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	store := model.New(db)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("applying schema: %v", err)
	}
	return store
}

func TestAgentCredentialPrefersTheSecretsDatabaseOverTheFile(t *testing.T) {
	ctx := context.Background()
	store := testSecrets(t)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Only the file: what a deployment seeded by scripts/setup.sh has
	// before anyone opens the UI.
	got, err := agentCredential(ctx, store, secrets.ClaudeOAuthTokenSecret, file)
	if err != nil || got != "from-the-file" {
		t.Fatalf("agentCredential = %q, %v; want the file's contents, trimmed", got, err)
	}

	// Then a key pasted into Settings: it wins, without the file having
	// to be rewritten or the daemon restarted.
	if err := store.Set(secrets.ClaudeOAuthTokenSecret, secrets.AgentCredentialKey, []byte("from-the-ui")); err != nil {
		t.Fatal(err)
	}
	got, err = agentCredential(ctx, store, secrets.ClaudeOAuthTokenSecret, file)
	if err != nil || got != "from-the-ui" {
		t.Fatalf("agentCredential = %q, %v; want the stored secret to win", got, err)
	}
}

func TestAgentCredentialIsEmptyWithNeitherSourceRatherThanAnError(t *testing.T) {
	ctx := context.Background()
	// A file path that names nothing is the same "not configured this
	// way" an unset flag is: scripts/setup.sh passes
	// -gemini-api-key-file unconditionally, pointing at a file it only
	// writes once there is a key to write.
	got, err := agentCredential(ctx, testSecrets(t), secrets.GeminiAPIKeySecret, filepath.Join(t.TempDir(), "never-written"))
	if err != nil || got != "" {
		t.Fatalf("agentCredential = %q, %v; want empty and no error", got, err)
	}
	got, err = agentCredential(ctx, nil, secrets.GeminiAPIKeySecret, "")
	if err != nil || got != "" {
		t.Fatalf("agentCredential with no store and no file = %q, %v; want empty and no error", got, err)
	}
}

func TestAgentFrameworksSaysWhereToSetAMissingCredential(t *testing.T) {
	ctx := context.Background()
	// -claude-path names a file that need not exist (claude.New does not
	// stat it) purely so the claude leg gets past exec.LookPath and
	// reaches the credential check this is about. Without it, the answer
	// depends on whether the machine running the test happens to have a
	// claude binary on $PATH: present, and the credential error asserted
	// below comes back; absent -- every CI runner -- and the framework
	// fails one step earlier, on the missing binary, which is a correct
	// error about a different missing thing.
	cfg := config{claudePath: filepath.Join(t.TempDir(), "claude")}
	build := agentFrameworks(cfg, testStore(t), testSecrets(t))

	for _, framework := range []string{model.AgentFrameworkGemini, model.AgentFrameworkClaude} {
		_, err := build(ctx, framework)
		if err == nil {
			t.Fatalf("building the %s framework with no credential succeeded", framework)
		}
		// The message is the whole point: this reaches an operator as a
		// setup-failed run in the UI, and it has to say what to do about
		// it there rather than name a flag they cannot reach.
		if !strings.Contains(err.Error(), "Settings -> Agent frameworks") {
			t.Fatalf("%s framework error = %v; want it to point at the UI", framework, err)
		}
	}
}

// The counterpart to the test above, for the other thing the claude
// framework needs and the Gemini one does not: a binary on the host.
// Nothing in the v2 deployment path installed it until scripts/setup.sh's
// own install_claude_cli, so this is the error a deployment that predates
// that -- or one whose install was blocked -- actually hits, once per
// dispatch, the moment someone selects "claude" in Settings.
func TestAgentFrameworksSaysHowToInstallAMissingClaudeCLI(t *testing.T) {
	// An empty $PATH, so this asserts the same thing whether or not the
	// machine running it happens to have a claude binary -- the reverse
	// of the -claude-path trick the credential test above uses.
	t.Setenv("PATH", t.TempDir())
	secretStore := testSecrets(t)
	if err := secretStore.Set(secrets.ClaudeOAuthTokenSecret, secrets.AgentCredentialKey, []byte("sk-ant-oat01-fake")); err != nil {
		t.Fatal(err)
	}

	_, err := agentFrameworks(config{}, testStore(t), secretStore)(context.Background(), model.AgentFrameworkClaude)
	if err == nil {
		t.Fatal("building the claude framework with no claude binary succeeded")
	}
	// "executable file not found in $PATH" alone reads as grain being
	// broken; an operator needs to be told this is a host missing a
	// package, and what puts it there.
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error = %v; want it to name the CLI as not installed", err)
	}
	if !strings.Contains(err.Error(), "setup.sh") {
		t.Errorf("error = %v; want it to point at the script that installs the CLI", err)
	}
}

func TestAgentFrameworksRejectsAnUnknownName(t *testing.T) {
	_, err := agentFrameworks(config{}, testStore(t), testSecrets(t))(context.Background(), "gpt")
	if err == nil || !strings.Contains(err.Error(), "unknown agent framework") {
		t.Fatalf("err = %v, want an unknown-framework error", err)
	}
}

func TestAgentFrameworksBuildsClaudeFromAUISetToken(t *testing.T) {
	ctx := context.Background()
	secretStore := testSecrets(t)
	if err := secretStore.Set(secrets.ClaudeOAuthTokenSecret, secrets.AgentCredentialKey, []byte("sk-ant-oat01-fake")); err != nil {
		t.Fatal(err)
	}
	// -claude-path, so this needs no claude binary on $PATH to prove the
	// credential half.
	cfg := config{claudePath: filepath.Join(t.TempDir(), "claude")}

	framework, err := agentFrameworks(cfg, testStore(t), secretStore)(ctx, model.AgentFrameworkClaude)
	if err != nil {
		t.Fatalf("building the claude framework: %v", err)
	}
	if _, ok := framework.(*claude.Framework); !ok {
		t.Fatalf("framework = %T, want *claude.Framework", framework)
	}
}

func TestDefaultAgentFrameworkFollowsTheStoredSetting(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	cfg := config{agentFramework: model.AgentFrameworkGemini}

	// Nothing stored yet: the flag's own value, which is what seeded the
	// row every other deployment already has.
	if got := defaultAgentFramework(ctx, store, cfg); got != model.AgentFrameworkGemini {
		t.Fatalf("defaultAgentFramework = %q with an empty store, want gemini", got)
	}

	if err := store.PutConfig(ctx, model.Config{
		PollInterval: 30_000_000_000, MaxConcurrent: 1,
		AgentFramework: model.AgentFrameworkClaude,
	}); err != nil {
		t.Fatal(err)
	}
	// Re-read, not cached at startup: switching the default in Settings
	// has to reach the next dispatch, since that is the whole promise the
	// pane makes.
	if got := defaultAgentFramework(ctx, store, cfg); got != model.AgentFrameworkClaude {
		t.Fatalf("defaultAgentFramework = %q after the store said claude, want claude", got)
	}
}

func TestLiveTranscriptsPicksTheFormatPerFile(t *testing.T) {
	dir := t.TempDir()
	transcripts := liveTranscriptDir(dir)

	// agent/claude mirrors claude's own --output-format stream-json...
	claudeLine := `{"type":"assistant","message":{"content":[{"type":"text","text":"looking at the parser"}]}}`
	if err := os.WriteFile(filepath.Join(dir, "run-claude"), []byte(claudeLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ...while agent/gemini tees an already-readable narrative.
	if err := os.WriteFile(filepath.Join(dir, "run-gemini"), []byte("thinking about the parser\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	text, ok, err := transcripts.Tail("run-claude")
	if err != nil || !ok {
		t.Fatalf("Tail(run-claude) = %q, %v, %v", text, ok, err)
	}
	if !strings.Contains(text, "looking at the parser") {
		t.Fatalf("Tail(run-claude) = %q, want the stream-json decoded", text)
	}
	if strings.Contains(text, `"type"`) {
		t.Fatalf("Tail(run-claude) = %q, want it parsed rather than handed back raw", text)
	}

	text, ok, err = transcripts.Tail("run-gemini")
	if err != nil || !ok || text != "thinking about the parser" {
		t.Fatalf("Tail(run-gemini) = %q, %v, %v; want the narrative trimmed", text, ok, err)
	}

	if text, ok, err := transcripts.Tail("run-that-never-started"); err != nil || ok || text != "" {
		t.Fatalf("Tail of a missing transcript = %q, %v, %v; want the not-yet reading", text, ok, err)
	}
}
