package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwsalmon/grain/pkg/agent"
)

// recordingRunner captures everything Framework.Run hands the subprocess,
// and replays a canned stream back. It is the seam for asserting on how
// agy is invoked -- what testing.go's scriptRunner deliberately does not
// do, since that one is busy actually running tools.
type recordingRunner struct {
	args   []string
	stdin  string
	env    []string
	dir    string
	stdout string
	// homeAtRun is a copy of the settings file found in the HOME this
	// run was given, read while the run is notionally in flight -- Run
	// deletes that directory as it returns, so a test cannot look
	// afterwards.
	homeAtRun string
}

func (r *recordingRunner) Run(_ context.Context, args []string, stdin string, env []string, dir string, tee io.Writer) (string, error) {
	r.args, r.stdin, r.env, r.dir = args, stdin, env, dir
	for _, e := range env {
		if home, ok := strings.CutPrefix(e, "HOME="); ok {
			if data, err := os.ReadFile(filepath.Join(home, settingsRelPath)); err == nil {
				r.homeAtRun = string(data)
			}
		}
	}
	if tee != nil {
		io.WriteString(tee, r.stdout)
	}
	return r.stdout, nil
}

func okStream() string {
	return stream(
		initLine,
		toolActive(0, "write_file", `{"path":"ok.txt"}`),
		toolDone(0, "write_file", "wrote ok.txt"),
		`{"event":"result","result":{"status":"SUCCESS","response":"done"}}`,
	)
}

// TestRunSendsThePromptOverStdinAndNeverInArgv is the discipline v1's
// dispatch.py set and agent/claude kept: untrusted issue content must
// never become a ps-visible argument. agy's other print mode takes the
// prompt as the argument to --print, which is exactly why this package
// uses --input-format stream-json instead.
func TestRunSendsThePromptOverStdinAndNeverInArgv(t *testing.T) {
	const secret = "untrusted-issue-body-that-must-not-reach-argv"
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: secret, SandboxRoot: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, a := range r.args {
		if strings.Contains(a, secret) {
			t.Fatalf("prompt reached argv (%q); it must travel over stdin only", a)
		}
	}
	var ev struct {
		Event   string `json:"event"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.stdin)), &ev); err != nil {
		t.Fatalf("stdin was not one JSON user event: %v (%q)", err, r.stdin)
	}
	if ev.Event != "user" || ev.Message.Role != "user" || ev.Message.Content != secret {
		t.Errorf("stdin user event = %+v, want the prompt as a user turn", ev)
	}
	if !argsHave(r.args, "--input-format", "stream-json") {
		t.Errorf("args = %v, want --input-format stream-json (the mode that reads the prompt from stdin)", r.args)
	}
	if argsHave(r.args, "--print", secret) {
		t.Errorf("args = %v, want no --print <prompt>", r.args)
	}
}

// TestRunWritesThisRunsOwnMCPSettingsIntoAPrivateHome is the property
// that stands in for claude's --mcp-config: agy registers MCP servers
// per-user, so two concurrent runs against two sandboxes could otherwise
// only ever see whichever registration was written last.
func TestRunWritesThisRunsOwnMCPSettingsIntoAPrivateHome(t *testing.T) {
	root := t.TempDir()
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain")

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: root}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var home string
	for _, e := range r.env {
		if h, ok := strings.CutPrefix(e, "HOME="); ok {
			home = h
		}
	}
	if home == "" {
		t.Fatal("no HOME in the subprocess environment; agy would read the controller's own ~/.gemini")
	}
	if home == os.Getenv("HOME") {
		t.Fatal("HOME was the controller's own; a run must get a private one")
	}
	if r.homeAtRun == "" {
		t.Fatalf("no %s written into the private HOME", settingsRelPath)
	}

	var settings struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(r.homeAtRun), &settings); err != nil {
		t.Fatalf("settings file was not JSON: %v (%q)", err, r.homeAtRun)
	}
	if len(settings.MCPServers) != 1 {
		t.Fatalf("mcpServers = %+v, want exactly one -- the only server this run may see", settings.MCPServers)
	}
	srv := settings.MCPServers[mcpServerName]
	if srv.Command != "/usr/local/bin/grain" {
		t.Errorf("mcp command = %q, want this grain binary", srv.Command)
	}
	if !argsHave(srv.Args, "-sandbox-root", root) || srv.Args[0] != "mcpserver" {
		t.Errorf("mcp args = %v, want the mcpserver subcommand rooted at this run's sandbox", srv.Args)
	}

	// The directory is the run's own, and goes away with it: nothing may
	// outlive a run holding a pointer at that run's sandbox.
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("private HOME %s still exists after Run returned (stat err = %v)", home, err)
	}
}

// TestRunPointsAKonturRunAtItsVMInsteadOfALocalRoot is the other half of
// mcpServerArgs, and the case where there is no local directory to name.
func TestRunPointsAKonturRunAtItsVMInsteadOfALocalRoot(t *testing.T) {
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain", WithKonturSSH("agent", "/keys/exec", "/workspace"))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", KonturVM: "grain-run-7"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var settings struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(r.homeAtRun), &settings); err != nil {
		t.Fatalf("settings file was not JSON: %v", err)
	}
	args := settings.MCPServers[mcpServerName].Args
	for _, want := range [][2]string{
		{"-kontur-vm", "grain-run-7"}, {"-ssh-user", "agent"},
		{"-exec-key", "/keys/exec"}, {"-workspace", "/workspace"},
	} {
		if !argsHave(args, want[0], want[1]) {
			t.Errorf("mcp args = %v, want %s %s", args, want[0], want[1])
		}
	}
	if argsHave(r.args, "--add-dir", "") {
		t.Errorf("args = %v, want no --add-dir for a kontur run: there is no local path to name", r.args)
	}
}

// TestRunRejectsAKonturVMWithoutSSHConfig keeps the failure at
// construction time rather than letting a forked mcpserver fail to reach
// anything.
func TestRunRejectsAKonturVMWithoutSSHConfig(t *testing.T) {
	f := newFramework(&recordingRunner{stdout: okStream()}, "/usr/local/bin/grain")
	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", KonturVM: "grain-run-7"})
	if err == nil {
		t.Fatal("Run err = nil for a kontur VM with no WithKonturSSH, want an error")
	}
	if result != nil {
		t.Errorf("Run result = %+v, want nil: this run never started", result)
	}
}

// TestRunRequiresASandbox is the same guard for a RunConfig naming
// neither a root nor a VM.
func TestRunRequiresASandbox(t *testing.T) {
	f := newFramework(&recordingRunner{stdout: okStream()}, "/usr/local/bin/grain")
	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go"}); err == nil {
		t.Fatal("Run err = nil with no sandbox at all, want an error")
	}
}

// TestRunPassesTheAPIKeyByEnvironmentNotArgv is WithAPIKey's own reason
// for existing: a credential in argv is a credential in `ps`.
func TestRunPassesTheAPIKeyByEnvironmentNotArgv(t *testing.T) {
	const key = "AIza-not-a-real-key"
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain", WithAPIKey(key))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: t.TempDir()}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, a := range r.args {
		if strings.Contains(a, key) {
			t.Fatalf("API key reached argv (%q)", a)
		}
	}
	var saw bool
	for _, e := range r.env {
		if e == "GEMINI_API_KEY="+key {
			saw = true
		}
	}
	if !saw {
		t.Errorf("env = %v, want GEMINI_API_KEY passed to the subprocess", r.env)
	}
}

// TestRunMirrorsTheRawStreamToTranscriptPath is what LiveTranscriptDir
// reads: the bytes on disk while a run is still going, not a narrative
// assembled once it ends.
func TestRunMirrorsTheRawStreamToTranscriptPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run-1")
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain")

	if _, err := f.Run(context.Background(), agent.RunConfig{
		Prompt: "go", SandboxRoot: t.TempDir(), TranscriptPath: path,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the mirrored transcript: %v", err)
	}
	if string(data) != okStream() {
		t.Errorf("mirrored transcript = %q, want the raw stream verbatim", data)
	}
	if got := PartialTranscript(string(data)); !strings.Contains(got, "wrote ok.txt") {
		t.Errorf("PartialTranscript of the mirror = %q, want the run's own narrative", got)
	}
}

// TestVerifyToolRosterNotesATooBroadRoster is the check standing in for
// claude's --strict-mcp-config, which agy has no equivalent of: a tool
// grain never published is reported to whoever reads the run.
func TestVerifyToolRosterNotesATooBroadRoster(t *testing.T) {
	capture := stream(
		`{"event":"init","init":{"tools":["mcp__grain-sandbox__run_command","Bash"]}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":"done"}}`,
	)
	unexpected := verifyToolRoster(capture)
	if len(unexpected) != 1 || unexpected[0] != "Bash" {
		t.Fatalf("verifyToolRoster = %v, want just the tool grain never published", unexpected)
	}

	r := &recordingRunner{stdout: capture}
	f := newFramework(r, "/usr/local/bin/grain")
	result, err := f.Run(context.Background(), agent.RunConfig{Prompt: "go", SandboxRoot: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result.Transcript, "Bash") {
		t.Errorf("Transcript = %q, want the unexpected tool named in it", result.Transcript)
	}
}

// TestVerifyToolRosterAcceptsExactlyWhatGrainPublishes is the other side:
// the roster a correctly wired run reports must produce no note at all,
// or the note above would be noise on every run.
func TestVerifyToolRosterAcceptsExactlyWhatGrainPublishes(t *testing.T) {
	tools, err := json.Marshal(allowedTools())
	if err != nil {
		t.Fatal(err)
	}
	capture := stream(
		`{"event":"init","init":{"tools":`+string(tools)+`}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":"done"}}`,
	)
	if unexpected := verifyToolRoster(capture); len(unexpected) != 0 {
		t.Errorf("verifyToolRoster = %v, want none for the roster grain itself publishes", unexpected)
	}
}

// The daemon's address and the run's task id are what the forked
// mcpserver needs to offer open_pull_request at all, and they only travel
// as its own arguments -- the same pair agent/claude passes, since both
// fork the identical server.
func TestRunPassesTheGrainServerAndTaskToTheMCPServer(t *testing.T) {
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain", WithGrainServer("http://127.0.0.1:8420"))

	if _, err := f.Run(context.Background(), agent.RunConfig{
		Prompt: "x", SandboxRoot: t.TempDir(), TaskID: "t1",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := settingsArgs(t, r.homeAtRun)
	if !argsHave(args, "-server", "http://127.0.0.1:8420") || !argsHave(args, "-task", "t1") {
		t.Errorf("mcpserver args = %v, want -server and -task among them", args)
	}
}

// Both or neither: one without the other names a question with nowhere
// to send it, and mcpserver rejects either half on its own.
func TestRunOmitsTheGrainServerWithoutATaskID(t *testing.T) {
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain", WithGrainServer("http://127.0.0.1:8420"))

	if _, err := f.Run(context.Background(), agent.RunConfig{
		Prompt: "x", SandboxRoot: t.TempDir(),
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := settingsArgs(t, r.homeAtRun)
	if argsHave(args, "-server", "") || argsHave(args, "-task", "") {
		t.Errorf("mcpserver args = %v, want neither -server nor -task", args)
	}
}

// open_pull_request is published like every other tool: agy has no
// --allowedTools of its own, so this list is what verifyToolRoster
// measures a run's reported roster against, and a tool missing from it
// would be reported as one grain never published.
func TestAllowedToolsNamesOpenPullRequest(t *testing.T) {
	names := allowedTools()
	for _, n := range names {
		if n == "mcp__grain-sandbox__open_pull_request" {
			return
		}
	}
	t.Errorf("allowedTools() = %v, want open_pull_request among them", names)
}

// settingsArgs pulls the grain-sandbox server's own argument list out of
// the settings file a run's private HOME was given.
func settingsArgs(t *testing.T, settingsJSON string) []string {
	t.Helper()
	var settings struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(settingsJSON), &settings); err != nil {
		t.Fatalf("settings file was not JSON: %v (%s)", err, settingsJSON)
	}
	return settings.MCPServers[mcpServerName].Args
}

func argsHave(args []string, name, value string) bool {
	for i, a := range args {
		if a == name && i+1 < len(args) && (value == "" || args[i+1] == value) {
			return true
		}
	}
	return false
}

// See the matching test in pkg/agent/claude: with `kontur run` generating
// a keypair per guest, no exec key is the normal configuration, and an
// empty "-exec-key" is worse than none at all.
func TestRunOmitsExecKeyWhenUnset(t *testing.T) {
	r := &recordingRunner{stdout: okStream()}
	f := newFramework(r, "/usr/local/bin/grain", WithKonturSSH("agent", "", "/workspace"))

	if _, err := f.Run(context.Background(), agent.RunConfig{Prompt: "x", KonturVM: "grain-run-7"}); err != nil {
		t.Fatalf("a kontur run with no exec key should work, got: %v", err)
	}
	var settings struct {
		MCPServers map[string]struct {
			Args []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(r.homeAtRun), &settings); err != nil {
		t.Fatalf("settings file was not JSON: %v", err)
	}
	for _, arg := range settings.MCPServers[mcpServerName].Args {
		if arg == "-exec-key" {
			t.Errorf("args = %v, want no -exec-key when none is configured", settings.MCPServers[mcpServerName].Args)
		}
	}
}
