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

	"github.com/bwsalmon/grain/pkg/agent/antigravity"
	"github.com/bwsalmon/grain/pkg/agent/claude"
	"github.com/bwsalmon/grain/pkg/model"
	"github.com/bwsalmon/grain/pkg/model/sqlite"
	"github.com/bwsalmon/grain/pkg/secrets"
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
	// -agy-path is stubbed for the same reason, both frameworks needing
	// a binary now that agent/antigravity runs the Antigravity CLI where
	// the in-process Gemini runtime needed nothing on the host.
	cfg := config{
		agyPath:    filepath.Join(t.TempDir(), "agy"),
		claudePath: filepath.Join(t.TempDir(), "claude"),
	}
	build := agentFrameworks(cfg, testStore(t), testSecrets(t))

	for _, framework := range []string{model.AgentFrameworkAntigravity, model.AgentFrameworkClaude} {
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

// The counterpart to the test above, for the other thing each framework
// needs: a binary to exec. Dockerfile carries both agent CLIs now
// (bwsalmon/agents#645), so on a real deployment this error means an
// image built without one rather than a host missing a package -- and it
// is still what a dispatch hits, once per run, the moment someone
// selects that framework in Settings.
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
	// broken; an operator needs to be told this is a missing install,
	// and where the binary is supposed to come from.
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error = %v; want it to name the CLI as not installed", err)
	}
	if !strings.Contains(err.Error(), "Dockerfile") {
		t.Errorf("error = %v; want it to name where the CLI comes from", err)
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
	cfg := config{agentFramework: model.AgentFrameworkAntigravity}

	// Nothing stored yet: the flag's own value, which is what seeded the
	// row every other deployment already has.
	if got := dispatchConfig(ctx, store, cfg).defaultAgentFramework(); got != model.AgentFrameworkAntigravity {
		t.Fatalf("defaultAgentFramework = %q with an empty store, want antigravity", got)
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
	if got := dispatchConfig(ctx, store, cfg).defaultAgentFramework(); got != model.AgentFrameworkClaude {
		t.Fatalf("defaultAgentFramework = %q after the store said claude, want claude", got)
	}
}

// The same missing-binary path for the other framework. agent/
// antigravity runs Google's Antigravity CLI as a subprocess, so a
// deployment that upgraded across the runtime replacement has a host
// that never installed one -- and unlike claude's, scripts/setup.sh does
// not install it (no verified installer to run), only warns. That makes
// this the error such a deployment actually hits, so it has to say what
// to do about it.
func TestAgentFrameworksSaysHowToInstallAMissingAgyCLI(t *testing.T) {
	// An empty $PATH, so this asserts the same thing whether or not the
	// machine running it happens to have an agy binary.
	t.Setenv("PATH", t.TempDir())
	secretStore := testSecrets(t)
	if err := secretStore.Set(secrets.GeminiAPIKeySecret, secrets.AgentCredentialKey, []byte("AIza-fake")); err != nil {
		t.Fatal(err)
	}

	_, err := agentFrameworks(config{}, testStore(t), secretStore)(context.Background(), model.AgentFrameworkAntigravity)
	if err == nil {
		t.Fatal("building the antigravity framework with no agy binary succeeded")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("error = %v; want it to name the CLI as not installed", err)
	}
	// Dockerfile carries agy now (bwsalmon/agents#645), which it did
	// not when this error was written: the message used to send an
	// operator off to install it by hand on the host, and the honest
	// answer on a real deployment is that the image was built without
	// it.
	if !strings.Contains(err.Error(), "Dockerfile") {
		t.Errorf("error = %v; want it to name where the CLI comes from", err)
	}
}

// A stored row or a task still carrying the framework's former name has
// to dispatch onto the framework that name now means, rather than into
// agentFrameworks' unknown-framework error. Nothing rewrites those rows
// (model.Config.AgentFramework's own doc comment), so this is the whole
// upgrade path for a deployment that set the framework before the
// rename.
func TestAgentFrameworksAcceptsTheLegacyGeminiName(t *testing.T) {
	cfg := config{agyPath: filepath.Join(t.TempDir(), "agy")}
	secretStore := testSecrets(t)
	if err := secretStore.Set(secrets.GeminiAPIKeySecret, secrets.AgentCredentialKey, []byte("AIza-fake")); err != nil {
		t.Fatal(err)
	}

	framework, err := agentFrameworks(cfg, testStore(t), secretStore)(
		context.Background(), model.LegacyAgentFrameworkGemini)
	if err != nil {
		t.Fatalf("building the framework named %q: %v", model.LegacyAgentFrameworkGemini, err)
	}
	if _, ok := framework.(*antigravity.Framework); !ok {
		t.Fatalf("framework = %T, want *antigravity.Framework", framework)
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
	// ...and agent/antigravity mirrors agy's, which is NDJSON too. The
	// two are told apart by the key each tags its events with -- "type"
	// above, "event" here -- since "does it open with a brace" stopped
	// separating them when the in-process Gemini runtime (which tee'd an
	// already-readable narrative) was replaced.
	agyLine := `{"event":"step_update","step_update":{"step_index":0,"state":"DONE",` +
		`"step_type":"agent_response","text_delta":"thinking about the parser"}}`
	if err := os.WriteFile(filepath.Join(dir, "run-agy"), []byte(agyLine+"\n"), 0o600); err != nil {
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

	text, ok, err = transcripts.Tail("run-agy")
	if err != nil || !ok || text != "thinking about the parser" {
		t.Fatalf("Tail(run-agy) = %q, %v, %v; want agy's stream-json decoded", text, ok, err)
	}
	if strings.Contains(text, `"event"`) {
		t.Fatalf("Tail(run-agy) = %q, want it parsed rather than handed back raw", text)
	}

	if text, ok, err := transcripts.Tail("run-that-never-started"); err != nil || ok || text != "" {
		t.Fatalf("Tail of a missing transcript = %q, %v, %v; want the not-yet reading", text, ok, err)
	}
}
