package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bwsalmon/grain/pkg/model"
)

// HTTPClient is Client's method surface (bwsalmon/agents#363) spoken over
// the wire instead of against a model.Store directly: cmd/grain's task
// CLI used to open the store itself -- embedded, or a Dolt SQL server via
// -store-addr -- and call Client's methods in-process, the same code path
// Server wraps in JSON for the frontend. That made the CLI a second
// direct writer alongside whatever daemon a deployment also runs, which
// is exactly the "single writer" caveat the store's own docs used to
// carry a whole section about. Now the daemon is the only thing that ever
// opens the store; the CLI is this instead, a plain REST client of the
// same JSON API the frontend already speaks, reaching it however an
// operator's network gets it there (a loopback port, an SSH tunnel,
// Tailscale, IAP -- HTTPClient does not care, and carries no credential
// of its own, matching the deployment's "no auth needed, the network is
// the perimeter" shape).
type HTTPClient struct {
	// BaseURL is the daemon's own address, e.g. "http://127.0.0.1:8420" --
	// no trailing slash required.
	BaseURL string
	// HTTP is the client requests are sent with. nil means
	// http.DefaultClient.
	HTTP *http.Client
}

// NewHTTPClient builds an HTTPClient against baseURL.
func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{BaseURL: strings.TrimRight(baseURL, "/")}
}

func (c *HTTPClient) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

// do sends a request with an optional JSON body and decodes a JSON
// response into out (nil to discard it), translating a non-2xx response
// into a NotFoundError, a ValidationError, or a plain error carrying the
// server's own message -- the same three shapes writeClientError maps a
// direct Client error to, so a CLI caller sees identical behaviour either
// way.
func (c *HTTPClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("reaching grain server at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return httpError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decoding response from %s %s: %w", method, path, err)
	}
	return nil
}

// httpError reads a {"error": "..."} body (writeError's own shape) and
// maps the status back to the same error type writeClientError derived
// it from -- 404 to NotFoundError, 400 to ValidationError, anything else
// to a plain error carrying the message and status.
func httpError(resp *http.Response) error {
	var payload struct {
		Error string `json:"error"`
	}
	message := resp.Status
	if err := json.NewDecoder(resp.Body).Decode(&payload); err == nil && payload.Error != "" {
		message = payload.Error
	}
	switch resp.StatusCode {
	case http.StatusNotFound:
		return &NotFoundError{message: message}
	case http.StatusBadRequest:
		return validationErrorf("%s", message)
	default:
		return fmt.Errorf("grain server: %s (%s)", message, resp.Status)
	}
}

func (c *HTTPClient) ListTasks(ctx context.Context) ([]Task, error) {
	var tasks []Task
	if err := c.do(ctx, http.MethodGet, "/api/tasks", nil, &tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

// Task returns one task's list-shaped view, fetched the same way GetTask
// is -- there is no narrower endpoint, since the frontend never needed
// one either.
func (c *HTTPClient) Task(ctx context.Context, id string) (Task, error) {
	detail, err := c.GetTask(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return detail.Task, nil
}

func (c *HTTPClient) GetTask(ctx context.Context, id string) (TaskDetail, error) {
	var detail TaskDetail
	if err := c.do(ctx, http.MethodGet, "/api/tasks/"+id, nil, &detail); err != nil {
		return TaskDetail{}, err
	}
	return detail, nil
}

func (c *HTTPClient) CreateTask(ctx context.Context, req CreateTaskRequest) (Task, error) {
	var task Task
	if err := c.do(ctx, http.MethodPost, "/api/tasks", req, &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (c *HTTPClient) UpdateTask(ctx context.Context, id string, req UpdateTaskRequest) (Task, error) {
	var task Task
	if err := c.do(ctx, http.MethodPatch, "/api/tasks/"+id, req, &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (c *HTTPClient) SetCapability(ctx context.Context, id, capabilityID string, attach bool) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/capabilities",
		setCapabilityRequest{ID: capabilityID, Attach: attach}, nil)
}

func (c *HTTPClient) Approve(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/approve", nil, nil)
}

func (c *HTTPClient) WithdrawApproval(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/withdraw-approval", nil, nil)
}

func (c *HTTPClient) AddComment(ctx context.Context, id, body string, attachments []AttachmentUpload) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/comments",
		addCommentRequest{Body: body, Attachments: attachments}, nil)
}

func (c *HTTPClient) Close(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/close", nil, nil)
}

func (c *HTTPClient) Reopen(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/reopen", nil, nil)
}

func (c *HTTPClient) Retry(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/retry", nil, nil)
}

// OpenPullRequest is Client.OpenPullRequest over the wire: open (or find)
// the pull request for id's own branch and read its current checks.
//
// Its caller is not the CLI but `grain mcpserver` (cmd/grain/mcpserver.go),
// which serves a dispatched run's own tools and holds no GitHub
// credential -- this is the hop that lets a run open its pull request
// while it is still running without a credential ever reaching the
// process the agent drives.
func (c *HTTPClient) OpenPullRequest(ctx context.Context, id string) (PullRequestStatus, error) {
	var status PullRequestStatus
	if err := c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/pull-request", nil, &status); err != nil {
		return PullRequestStatus{}, err
	}
	return status, nil
}

// RecreateSandbox is Client.RecreateSandbox over the wire: destroy the
// sandbox of id's live run and build an empty one in its place.
//
// Its caller, like OpenPullRequest's above, is not the CLI but `grain
// mcpserver` (cmd/grain/mcpserver.go), which serves a dispatched run's
// own tools. Everything that process can do happens *inside* the
// sandbox, so throwing that sandbox away and building another is the one
// thing it has to ask the daemon for.
func (c *HTTPClient) RecreateSandbox(ctx context.Context, id string) (SandboxRecreation, error) {
	var recreation SandboxRecreation
	if err := c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/sandbox/recreate", nil, &recreation); err != nil {
		return SandboxRecreation{}, err
	}
	return recreation, nil
}

// Config reads the deployment's fixed configuration -- who the daemon
// attributes every task and comment written through this API to, its
// default target repo, and the capabilities it offers. Unlike the
// store-backed Client, an HTTPClient has no -as flag of its own to
// override this with: attribution is a deployment-wide setting now,
// carried by whichever principal the daemon itself was started as,
// matching the "no auth, the network is the perimeter" model this
// deployment shape assumes rather than distinguishing callers.
func (c *HTTPClient) Config(ctx context.Context) (Config, error) {
	var resp configResponse
	if err := c.do(ctx, http.MethodGet, "/api/config", nil, &resp); err != nil {
		return Config{}, err
	}
	cfg := Config{
		Actor:        model.Principal{Kind: model.PrincipalKind(resp.ActorKind), ID: resp.Actor},
		Capabilities: resp.Capabilities,
		TargetRepos:  resp.TargetRepos,
	}
	if resp.DefaultTarget != "" {
		target, err := model.ParseRepo(resp.DefaultTarget)
		if err != nil {
			return Config{}, fmt.Errorf("server returned an unparseable default target %q: %w", resp.DefaultTarget, err)
		}
		cfg.DefaultTarget = &target
	}
	return cfg, nil
}

func (c *HTTPClient) GetSettings(ctx context.Context) (Settings, error) {
	var settings Settings
	if err := c.do(ctx, http.MethodGet, "/api/settings", nil, &settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

func (c *HTTPClient) UpdateSettings(ctx context.Context, req UpdateSettingsRequest) (Settings, error) {
	var settings Settings
	if err := c.do(ctx, http.MethodPut, "/api/settings", req, &settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// The repo family (grain/task-36): what a repo defaults on its own, and
// whether the deployment's allowlist names it. Four of the five mirror
// the Client method of the same name -- the ones the repos pane already
// calls in-process -- and ListRepos has no Client counterpart because it
// composes two endpoints that already exist rather than reaching a new
// one. The types they speak are repos.go's, next to the Client methods
// and HTTP handlers they cross the wire between; only the wire calls
// themselves live here, where every other HTTPClient method does.

// ListRepos reports one row per repo this deployment knows about --
// RepoSummary's own doc comment for why that is derived from GET
// /api/config and GET /api/tasks here rather than served whole by an
// endpoint of its own.
//
// Two requests, not one, and deliberately not concurrent: the second is
// a plain list of tasks, the ordering between them does not matter, and
// a CLI list command is not where saving one round trip is worth a
// goroutine and a second error path.
func (c *HTTPClient) ListRepos(ctx context.Context) ([]RepoSummary, error) {
	var cfg configResponse
	if err := c.do(ctx, http.MethodGet, "/api/config", nil, &cfg); err != nil {
		return nil, err
	}
	tasks, err := c.ListTasks(ctx)
	if err != nil {
		return nil, err
	}
	return repoSummaries(cfg.TargetRepos, tasks, cfg.RepoDefaultCapabilities, cfg.ReposWithPromptExtension), nil
}

// RepoDefaults reads repo's own default capability set, alongside the
// deployment-wide one it composes with and the effective set that
// composition produces -- Client.RepoDefaults over the wire.
func (c *HTTPClient) RepoDefaults(ctx context.Context, repo string) (RepoDefaults, error) {
	path, err := repoAPIPath(repo, "/capabilities")
	if err != nil {
		return RepoDefaults{}, err
	}
	var defaults RepoDefaults
	if err := c.do(ctx, http.MethodGet, path, nil, &defaults); err != nil {
		return RepoDefaults{}, err
	}
	return defaults, nil
}

// SetRepoDefaultCapabilities replaces repo's own default capability set
// -- Client.SetRepoDefaultCapabilities over the wire, including its
// refusal of an id no capability answers to. A nil or empty ids is how a
// repo is returned to adding nothing, the same as sending an empty list
// from the repos pane.
func (c *HTTPClient) SetRepoDefaultCapabilities(ctx context.Context, repo string, ids []string) (RepoDefaults, error) {
	path, err := repoAPIPath(repo, "/capabilities")
	if err != nil {
		return RepoDefaults{}, err
	}
	var defaults RepoDefaults
	if err := c.do(ctx, http.MethodPut, path, SetRepoCapabilitiesRequest{DefaultCapabilities: ids}, &defaults); err != nil {
		return RepoDefaults{}, err
	}
	return defaults, nil
}

// SetRepoPromptExtension replaces repo's own standing instructions --
// Client.SetRepoPromptExtension over the wire. An empty text is how a
// repo goes back to adding nothing of its own, the same as clearing the
// box on the repos pane; everything else about the repo, its default
// capabilities included, is left exactly as it was.
func (c *HTTPClient) SetRepoPromptExtension(ctx context.Context, repo, text string) (RepoDefaults, error) {
	path, err := repoAPIPath(repo, "/prompt-extension")
	if err != nil {
		return RepoDefaults{}, err
	}
	var defaults RepoDefaults
	if err := c.do(ctx, http.MethodPut, path, SetRepoPromptExtensionRequest{PromptExtension: text}, &defaults); err != nil {
		return RepoDefaults{}, err
	}
	return defaults, nil
}

// AddTargetRepo appends repo to the deployment's TargetRepos allowlist
// and returns the settings that result -- Client.AddTargetRepo over the
// wire, idempotent the same way.
//
// Note what an empty allowlist means everywhere else it appears
// (Config.TargetRepos): unrestricted. So the *first* repo added to a
// deployment that has never restricted itself does not widen anything --
// it narrows the deployment to that one repo. `grain repo add` says so
// when the list it prints back has exactly one entry.
func (c *HTTPClient) AddTargetRepo(ctx context.Context, repo string) (Settings, error) {
	var settings Settings
	if err := c.do(ctx, http.MethodPost, "/api/repos", AddRepoRequest{Repo: repo}, &settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// RemoveTargetRepo removes repo from the deployment's TargetRepos
// allowlist -- Client.RemoveTargetRepo over the wire, a no-op rather
// than an error when repo isn't on it. It leaves everything else about
// the repo alone: tasks already targeting it keep doing so, and whatever
// it defaults on its own stays stored.
func (c *HTTPClient) RemoveTargetRepo(ctx context.Context, repo string) (Settings, error) {
	path, err := repoAPIPath(repo, "")
	if err != nil {
		return Settings{}, err
	}
	var settings Settings
	if err := c.do(ctx, http.MethodDelete, path, nil, &settings); err != nil {
		return Settings{}, err
	}
	return settings, nil
}

// repoAPIPath builds /api/repos/{owner}/{name} + suffix, parsing repo
// first so a malformed one fails here, as the same ValidationError the
// Client method of the same name would return, rather than as whatever
// the resulting URL happened to hit: "widgets" with no owner would
// otherwise build /api/repos/widgets/capabilities and come back as a
// bare 404 from a route it could never have matched.
func repoAPIPath(repo, suffix string) (string, error) {
	parsed, err := model.ParseRepo(repo)
	if err != nil {
		return "", validationErrorf("repo: %v", err)
	}
	return "/api/repos/" + url.PathEscape(parsed.Owner) + "/" + url.PathEscape(parsed.Name) + suffix, nil
}

// Metrics fetches a throughput and latency report over the window ending
// now. window is sent as the daemon's own ?window= string rather than a
// Go duration in nanoseconds, so a request made by hand (`curl
// '.../api/metrics?window=7d'`) and one made through here read the same;
// "" asks for the server's own default.
func (c *HTTPClient) Metrics(ctx context.Context, window string) (MetricsReport, error) {
	path := "/api/metrics"
	if window = strings.TrimSpace(window); window != "" {
		path += "?window=" + url.QueryEscape(window)
	}
	var report MetricsReport
	if err := c.do(ctx, http.MethodGet, path, nil, &report); err != nil {
		return MetricsReport{}, err
	}
	return report, nil
}
