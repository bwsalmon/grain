package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bwsalmon/grain/v2/pkg/model"
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

func (c *HTTPClient) SetDependency(ctx context.Context, id, dependsOnID string, attach bool) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/depends-on",
		setDependencyRequest{ID: dependsOnID, Attach: attach}, nil)
}

func (c *HTTPClient) Approve(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/approve", nil, nil)
}

func (c *HTTPClient) Submit(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/tasks/"+id+"/submit", nil, nil)
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
