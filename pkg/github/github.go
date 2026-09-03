// Package github is the GitHub REST calls a grain deployment needs to run
// GitHub-driven work: list labelled issues, move labels, confirm a branch
// exists, open a PR, read a PR's own data and its review comments, relay a
// human's reply. Ported from v1's own Python github module, carrying its
// design history (v1 roadmap items 2, 8, 9, 12, 13, and
// bwsalmon/agents#154's create_review) forward unchanged; nothing here
// revisits those decisions.
//
// Still no sandbox-side access -- docs/design.md's split surface
// ("Orchestrator: API operations... sandboxes: git transport only") holds
// exactly as it did in v1. Only the thing that calls this package changes
// once v2 has an executor to call it from; the package itself does not
// know or care who its caller is.
//
// create_review posts a **draft** review: the request body carries no
// event key, which is what keeps GitHub from submitting it (GitHub's own
// "Create a review for a pull request" reference -- omitting event leaves
// the review PENDING, visible only to the credential that created it,
// until a human opens it on github.com and submits it themselves). An
// agent posting a *submitted* review of its own code would be marking its
// own homework; a draft is a human's opinion of what an agent found,
// waiting on that same human's sign-off before anyone else sees it.
//
// Field shapes below (head.ref/head.sha, base.ref, and the review-comment
// object's id/user.login/body/path/line) are pinned against GitHub's own
// REST reference (GET .../pulls/{number}, GET .../pulls/{number}/comments),
// not guessed.
package github

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ApiResponse is one HTTP response, decoupled from net/http so a Transport
// can be satisfied by something that never opens a socket (FakeTransport,
// githubsim.Sim).
type ApiResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Transport sends one already-built request and returns the raw response.
// The seam GitHubClient's own logic (path building, pagination, status
// handling, JSON field extraction) is testable through, with no real call
// to api.github.com -- the same shape gitproxy.Forwarder wraps http.Client
// in one layer down.
type Transport interface {
	Request(method, path string, headers map[string]string, body []byte) (ApiResponse, error)
}

// RealTransport talks to the GitHub API over HTTPS by default. UseTLS
// false exists for a Client pointed at a local mock server for a live
// end-to-end test -- a mock has no clean answer for a self-signed cert,
// and this package already treats "point it at a mock" as a first-class
// test seam (Transport itself, FakeTransport, githubsim.Sim) rather than
// something to fake with disabled certificate verification.
type RealTransport struct {
	Host   string
	UseTLS bool
	Client *http.Client
}

// NewRealTransport returns a transport aimed at host's REST API over
// HTTPS. host is a *git* host -- what a clone URL names -- and APIHost
// turns it into the host that actually answers REST paths.
func NewRealTransport(host string) *RealTransport {
	return &RealTransport{Host: APIHost(host), UseTLS: true}
}

// APIHost maps a GitHub git host to the host serving its REST API.
//
// For github.com those are two different names: git lives on github.com,
// the REST API on api.github.com. Everything in this package builds paths
// like "/repos/{owner}/{repo}/branches/{branch}", which are API paths, so
// a transport pointed at github.com sends them to
// https://github.com/repos/... -- a URL that has no API behind it and
// answers 404 to every one of them.
//
// That 404 is indistinguishable from a real one, and BranchExists
// (deliberately) reads 404 as "no such branch". A daemon configured with
// the correct git host therefore reported every branch as unpushed:
// pushes succeeded, since the git proxy forwards to the same host and for
// git that host is right, and then the run that had just pushed was
// recorded as having done nothing. No test caught it because the live
// tests point this at a githubsim on 127.0.0.1:port, which serves API
// paths at its root -- the one shape where git host and API host coincide.
//
// Anything that is not github.com is left alone: that is either such a
// mock or a GitHub Enterprise host. Enterprise is not actually supported
// by this mapping -- its API lives under a /api/v3 path prefix, not on a
// separate hostname, and Transport has no place to put a base path yet.
// Naming that here rather than silently pretending otherwise.
func APIHost(host string) string {
	if host == "github.com" {
		return "api.github.com"
	}
	return host
}

func (t *RealTransport) Request(method, path string, headers map[string]string, body []byte) (ApiResponse, error) {
	scheme := "https"
	if !t.UseTLS {
		scheme = "http"
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, scheme+"://"+t.Host+path, bodyReader)
	if err != nil {
		return ApiResponse{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return ApiResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ApiResponse{}, err
	}
	respHeaders := map[string]string{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}
	return ApiResponse{Status: resp.StatusCode, Headers: respHeaders, Body: data}, nil
}

// FakeCall is one recorded call to FakeTransport.Request.
type FakeCall struct {
	Method  string
	Path    string
	Headers map[string]string
	Body    []byte
}

// FakeTransport replays scripted responses in order. For unit tests,
// including pagination -- a call site that needs more than one page
// queues one response per page. Default answers every call once
// Responses is empty, rather than erroring, so a test that doesn't care
// about a particular call's answer need not script one.
type FakeTransport struct {
	Responses []ApiResponse
	Default   ApiResponse
	Calls     []FakeCall
}

// NewFakeTransport returns a FakeTransport whose Default is an empty JSON
// array -- the common case for a list endpoint a test doesn't care about.
func NewFakeTransport(responses ...ApiResponse) *FakeTransport {
	return &FakeTransport{Responses: responses, Default: ApiResponse{Status: 200, Body: []byte("[]")}}
}

func (t *FakeTransport) Request(method, path string, headers map[string]string, body []byte) (ApiResponse, error) {
	t.Calls = append(t.Calls, FakeCall{Method: method, Path: path, Headers: headers, Body: body})
	if len(t.Responses) > 0 {
		resp := t.Responses[0]
		t.Responses = t.Responses[1:]
		return resp, nil
	}
	return t.Default, nil
}

// TokenSource resolves the API token to use for one repo. A structural
// interface, not a named implementation grain/proxy's credential ladder
// has to declare conformance to -- anything with this method satisfies it.
type TokenSource interface {
	TokenFor(owner, repo string) *string
}

// StaticToken is one token regardless of repo -- what NewClient wraps a
// bare *string in when a caller has no per-repo ladder to consult.
type StaticToken struct {
	Token *string
}

func (s StaticToken) TokenFor(owner, repo string) *string { return s.Token }

// Error is the error a non-2xx GitHub response is reported as.
type Error struct {
	Status int
	Body   []byte
}

func (e *Error) Error() string {
	body := e.Body
	if len(body) > 200 {
		body = body[:200]
	}
	return fmt.Sprintf("GitHub API error %d: %q", e.Status, body)
}

// IsPermissionDenied reports whether err is GitHub refusing the call for
// want of a permission the credential can never hold -- a 403 whose body
// says "Resource not accessible by personal access token" (or "... by
// integration", the same refusal phrased for an App).
//
// It exists because one endpoint here can be unreachable by a whole
// class of credential: reading the Checks API needs the `repo` scope on
// a classic personal access token, or the "Checks" repository permission
// on a fine-grained one. A deployment whose credential has neither gets
// a 403 from ListCheckRuns on every call -- unlike an ordinary transient
// error, retrying never helps.
//
// The status alone is deliberately not enough. GitHub answers 403 for
// several conditions that are nothing to do with a missing permission
// and that do clear on their own or with a configuration change an
// operator can make: primary and secondary rate limits, SAML enforcement
// on an unauthorized token, and an organization IP allow list. Reading
// any of those as a permanent property of the credential would have a
// single transient 403 silently switch auto-merge off for the rest of
// the process's life (orchestrator.ChecksUnavailable latches), so
// anything this cannot positively identify as a missing permission stays
// an ordinary error, to fail loudly and be retried on the next cycle.
//
// A caller that can carry on without whatever the call would have told
// it should treat this as "unknown", never as "nothing found": the two
// differ exactly where it matters, since a 403 read as an empty result
// is indistinguishable from a genuinely clean answer.
func IsPermissionDenied(err error) bool {
	var e *Error
	if !errors.As(err, &e) || e.Status != 403 {
		return false
	}
	var body struct {
		Message string `json:"message"`
	}
	// An unparseable or message-less body is not a permission denial we
	// can vouch for, so it stays an error -- the safe direction, since
	// the cost of a false negative here is one retried cycle and the
	// cost of a false positive is auto-merge off until a restart.
	if err := json.Unmarshal(e.Body, &body); err != nil {
		return false
	}
	return strings.HasPrefix(body.Message, "Resource not accessible by")
}

// Issue is one issue or pull request from the issues endpoint (GitHub
// unifies the two; ListIssues filters pull requests out itself).
type Issue struct {
	Number  int
	Title   string
	Body    string
	HTMLURL string
	Labels  map[string]struct{}
	// State is GitHub's own field, "open" or "closed" -- read by a
	// cancel-on-close poll to tell a task issue a human closed early from
	// one still open.
	State string
	// Author is the issue's own opening account's login -- who a poll
	// attributes a freshly filed model.Task to, since nothing else names
	// the actor a labelled issue's own filing should be credited to.
	Author string
}

// HasLabel reports whether name is one of Issue's labels.
func (i Issue) HasLabel(name string) bool {
	_, ok := i.Labels[name]
	return ok
}

func labelSet(names ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// PullRequest is a freshly created (or found) pull request -- just enough
// to record and log. PullRequestDetail (below) is the wider shape a
// dispatch against an existing PR needs.
type PullRequest struct {
	Number  int
	HTMLURL string
}

// BranchHead is the tip of a branch: its sha and that commit's own
// message -- so a freshly opened PR's body can be seeded with the agent's
// own account of its change, the commit message it already wrote to
// explain the diff, rather than a generic, content-free line.
type BranchHead struct {
	SHA     string
	Message string
}

// PullRequestDetail is enough of a PR object to dispatch against it --
// title/body for the prompt, HeadRef for the branch to check out and push
// back to. Deliberately a separate type from PullRequest: widening that
// one would ripple into every existing call site for no benefit, since
// nothing there needs the extra fields.
type PullRequestDetail struct {
	Number  int
	Title   string
	Body    string
	HTMLURL string
	HeadRef string
	// HeadSHA is the PR head branch's current tip. Free on this same
	// response, and what scopes a workflow-run read to the commit the PR
	// actually points at -- filtering Actions runs by branch instead
	// would let a run from an older commit stand in for one the newest
	// push has not triggered yet, which is exactly the stale "passing"
	// that must never reach the merge gate.
	HeadSHA string
	BaseRef string
	// State is GitHub's own field, "open" or "closed" -- "closed" covers
	// both merged and closed-without-merging: a caller closing out a
	// finished PR treats them the same way (either one means nobody is
	// going to push more commits to it), but Merged below still tells
	// them apart for a caller that cares which.
	State string
	// Merged is GitHub's own field, meaningful only once State is
	// "closed" -- false the whole time a PR is open, same as GitHub's own
	// API returns. It is what lets a caller distinguish a merged PR from
	// one closed without merging, a distinction State alone collapses.
	Merged bool
	// MergedAt is GitHub's own field: when the PR merged, nil until
	// Merged is true. GitHub also returns it nil for a PR closed without
	// merging, which is the same nil this stays for that case.
	MergedAt *time.Time
	// CreatedAt is GitHub's own field: when the PR was opened. Present
	// for every PR regardless of state, unlike MergedAt.
	CreatedAt time.Time
	// Mergeable is GitHub's own field: true/false once it has finished
	// computing whether this PR can merge cleanly against its base, nil
	// while that computation is still in flight -- GitHub does this
	// asynchronously, so a request right after a push can legitimately see
	// nil for a cycle or two. A caller should read nil the same as "don't
	// know yet, check again next cycle," never as either a conflict or a
	// clean merge, since guessing either way could file a needless fix or
	// auto-merge something broken.
	Mergeable *bool
	// MergeableState is GitHub's own summary of why a PR can or cannot
	// merge -- "clean", "unstable", "blocked", "dirty", "behind",
	// "draft", "unknown". Parsed and carried because it needs no
	// permission beyond the Pull requests read this response already
	// costs, unlike either CI read.
	//
	// Deliberately NOT wired into the merge gate -- now for a measured
	// reason rather than an open question. Confirmed against live pull
	// requests: bwsalmon/grain#544, a branch carrying one check that goes
	// red in seconds beside one that runs for five minutes, polled every
	// five seconds from the moment it opened until every check had
	// finished, and bwsalmon/agents#655 for the empty case below.
	//
	// It does account for check runs, not merely for the older commit
	// statuses. Every reading was taken on commits carrying *zero*
	// commit statuses -- Actions creates check runs and nothing else, and
	// /commits/{sha}/status stayed empty the whole run -- and a check
	// still running or finished red read "unstable" throughout, never
	// "clean". So trusting it would not have auto-merged a PR with red
	// CI, which was the question that kept it out of the gate. The other
	// half of that reading came later on the same pull request, once the
	// red check was actually repaired: with all six checks completed and
	// green it read "clean". So where checks exist, "clean" and
	// "unstable" do separate a passing run from a broken one.
	//
	// It still cannot replace defaultCheckRegistrationWindow, which was
	// the reason to want it. On a pull request no workflow watches --
	// agents#655, whose diff touches no path that repo's only
	// pull_request workflow is filtered to -- mergeable_state is "clean"
	// with zero check runs and zero statuses, and stayed "clean" for
	// thirty consecutive reads over two and a half minutes. That is
	// byte-identical to a repo whose CI has merely not registered yet,
	// because it is computed from the same empty list: GitHub is no
	// better placed to tell those apart than the Checks API is. The
	// clock in orchestrator.healthFrom stays.
	//
	// That window was caught open rather than argued from. Polling
	// grain#544 once a second across a push, on this repository, whose
	// every push runs CI: two seconds after the new head appeared the PR
	// read "unknown" with no checks; two seconds after that it read
	// "clean" with *zero* check runs; two seconds after that the first
	// check registered and it went back to "unstable". A merge gate
	// reading "clean" as passing would have had a four-second window,
	// per push, to merge a pull request whose tests had not been created
	// yet -- which is the exact failure defaultCheckRegistrationWindow
	// exists to prevent, not a reason to retire it.
	//
	// Nor does "unstable" separate pending from failing -- a check still
	// running and a check finished red both read "unstable" -- so it
	// could not stand in for the PrPending/PrFailing distinction either.
	//
	// Two further readings from the same run, for whoever reaches for
	// this next. It is "unknown", with Mergeable nil, for a second or two
	// after every push while GitHub recomputes, and "unknown" again once
	// the PR is closed or merged -- so it can never serve the
	// merged/closed distinction Merged draws. A conflicted PR reads
	// "dirty" (observed on grain#545), agreeing with Mergeable false.
	//
	// One caveat the run could not cover: grain's own repository requires
	// no status check, which is why a red check reads "unstable" there.
	// Where checks are required the same failure reads "blocked", and so
	// does a PR waiting on a required review -- so "blocked" carries more
	// than CI, and a caller reading these values across arbitrary target
	// repos cannot treat one repo's vocabulary as every repo's.
	MergeableState string
}

// Comment is a plain top-level comment on an issue or PR -- GitHub's own
// /issues/{number}/comments endpoint serves both, since a PR is a special
// kind of issue in its data model. Distinct from ReviewComment: those are
// inline, diff-attached; this is the ordinary conversation thread -- where
// a human's reply to an agent's ask_question call actually lands.
type Comment struct {
	ID int
	// User is the commenting account's login.
	User string
	Body string
	// AuthorAssociation is GitHub's own field ("OWNER", "MEMBER",
	// "COLLABORATOR", "CONTRIBUTOR", "NONE", ...) -- what lets a caller
	// tell a trusted reply from an arbitrary public comment before acting
	// on it, the same trust tier as "can apply a label." A random
	// commenter on a public repo must not be able to redispatch the agent
	// with content of their choosing -- that would reopen the exact
	// prompt-injection gate the trigger label exists to close.
	AuthorAssociation string
}

// ReviewComment is one inline (diff-attached) review comment -- GitHub's
// own term for what GET .../pulls/{number}/comments returns, distinct
// from a plain top-level issue/PR comment. Line is nil for a comment
// GitHub considers outdated (the API returns original_line instead in
// that case) -- left nil here rather than falling back to original_line,
// since telling the agent "line N" about a line the diff has since moved
// past would be actively misleading.
type ReviewComment struct {
	ID   int
	User string
	Body string
	Path string
	Line *int
}

// NewReviewComment is one inline comment to attach to a draft review
// (Client.CreateReview) -- the input shape for what ReviewComment above
// is the output shape of. A separate type rather than reusing
// ReviewComment itself: ID and User are GitHub's to assign once the
// review is created, never a caller's to supply.
//
// Line is the line number in the file's *new* version (the right-hand
// side of the diff) that GitHub's own reviews API expects a comments[]
// entry to name -- the same "new" side ReviewComment.Line already reads
// back once a comment exists.
type NewReviewComment struct {
	Path string
	Line int
	Body string
}

// CheckRun is one check run against a commit -- GitHub's own shape for CI
// results (a GitHub Actions job, or any third-party check that posts
// through the Checks API), read to decide whether an open PR's tests are
// failing.
//
// Status is GitHub's own lifecycle field: "queued", "in_progress", or
// "completed" -- only "completed" has a meaningful Conclusion at all,
// which is why a caller checks Status before ever looking at it.
// Conclusion is nil until then, and one of "success", "failure",
// "neutral", "cancelled", "skipped", "timed_out", or "action_required"
// once it is -- GitHub's own enum, not narrowed here, since which of
// those count as "broken" is a policy decision for the caller, not this
// package.
type CheckRun struct {
	Name       string
	Status     string
	Conclusion *string
}

// JobLog is one failed GitHub Actions job and the tail of what it
// printed -- FailedJobLogs' unit.
//
// A check run says only that a job called "go" did not pass. That is
// enough to know a pull request is red and not enough to do anything
// about it, which is the gap this closes: the agent the merge queue
// files a fix task for reads the failure itself, in the fix task's own
// body, rather than being told a job name and left to go and find out
// what it said.
type JobLog struct {
	Name string
	// URL is the job's own page on github.com -- where the whole log
	// lives, for a reader who needs more than the tail below.
	URL string
	// Log is the last JobLogTailBytes of the job's log at most, cut at a
	// line boundary. The tail rather than the head because a job's log is
	// the whole job, every step of it, and the thing that broke is at the
	// end of it.
	Log string
	// Truncated reports whether Log is a tail rather than the whole log,
	// so a caller rendering it can say so.
	Truncated bool
}

// JobLogTailBytes is how much of one job's log FailedJobLogs keeps. Big
// enough for a Go test failure's own output plus the surrounding
// package lines; small enough that four of them stay something a person
// (and a model's context window) can read.
const JobLogTailBytes = 16 << 10

// maxFailedJobLogs bounds how many failed jobs one FailedJobLogs call
// reads logs for. A commit that failed more jobs than this has something
// wrong that reading a fifth log will not add to.
const maxFailedJobLogs = 4

// failedConclusion reports whether a completed run or job's conclusion is
// one of the three GitHub uses for "this did not pass" --
// orchestrator.failingChecks reads the same three, and the two lists
// have to keep agreeing: a job whose conclusion made a pull request
// PrFailing there but not "failed" here is one the fix task names and
// carries no log for.
func failedConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "startup_failure":
		return true
	}
	return false
}

// nextPagePath is the path+query of the rel="next" link, or "" on the
// last page.
//
// GitHub paginates via a full Link header URL rather than an opaque
// cursor; Transport.Request takes a path against a fixed host, so only
// the path and query survive the hop.
func nextPagePath(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	for _, part := range strings.Split(linkHeader, ",") {
		segment := strings.TrimSpace(part)
		if !strings.Contains(segment, `rel="next"`) {
			continue
		}
		start := strings.Index(segment, "<")
		end := strings.Index(segment, ">")
		if start == -1 || end == -1 || end < start {
			continue
		}
		u, err := url.Parse(segment[start+1 : end])
		if err != nil {
			continue
		}
		if u.RawQuery != "" {
			return u.Path + "?" + u.RawQuery
		}
		return u.Path
	}
	return ""
}

// Client is the GitHub operations a grain deployment needs. RESTClient is
// the real implementation, talking through a Transport; DryRunClient
// wraps one, printing instead of firing every mutation.
type Client interface {
	ListIssues(owner, repo, label string) ([]Issue, error)
	GetIssue(owner, repo string, number int) (Issue, error)
	AddLabel(owner, repo string, number int, label string) error
	RemoveLabel(owner, repo string, number int, label string) error
	CloseIssue(owner, repo string, number int) error
	ReopenIssue(owner, repo string, number int) error
	UpdateIssue(owner, repo string, number int, title, body *string) error
	BranchExists(owner, repo, branch string) (bool, error)
	GetBranchHead(owner, repo, branch string) (*BranchHead, error)
	CreateBranch(owner, repo, branch, sha string) error
	UpdateBranch(owner, repo, branch, sha string, force bool) error
	CreatePullRequest(owner, repo, head, base, title, body string) (PullRequest, error)
	FindOpenPullRequestForBranch(owner, repo, branch string) (*PullRequest, error)
	CreateIssue(owner, repo, title, body string, labels []string) (Issue, error)
	MergePullRequest(owner, repo string, number int, headSHA string) error
	GetPullRequest(owner, repo string, number int) (PullRequestDetail, error)
	DefaultBranch(owner, repo string) (string, error)
	ListReviewComments(owner, repo string, number int) ([]ReviewComment, error)
	ListCheckRuns(owner, repo, ref string) ([]CheckRun, error)
	ListWorkflowRuns(owner, repo, headSHA string) ([]CheckRun, error)
	FailedJobLogs(owner, repo, headSHA string) ([]JobLog, error)
	ListComments(owner, repo string, number int) ([]Comment, error)
	CreateComment(owner, repo string, number int, body string) (int, error)
	CreateReview(owner, repo string, number int, body string, comments []NewReviewComment) (int, error)
}

// RESTClient is Client talking to a real (or faked-at-the-Transport-level)
// GitHub REST API.
type RESTClient struct {
	Transport Transport
	Tokens    TokenSource
}

// NewClient returns a RESTClient. tokens may be nil, meaning every
// request goes out unauthenticated -- a fine, deliberate credential shape
// for a public repo, not an error case.
func NewClient(transport Transport, tokens TokenSource) *RESTClient {
	return &RESTClient{Transport: transport, Tokens: tokens}
}

func (c *RESTClient) headers(owner, repo string, jsonBody bool) map[string]string {
	headers := map[string]string{
		"Accept":               "application/vnd.github+json",
		"User-Agent":           "grain-automation",
		"X-GitHub-Api-Version": "2022-11-28",
	}
	var token *string
	if c.Tokens != nil {
		token = c.Tokens.TokenFor(owner, repo)
	}
	if token != nil {
		headers["Authorization"] = "token " + *token
	}
	if jsonBody {
		headers["Content-Type"] = "application/json"
	}
	return headers
}

func (c *RESTClient) get(owner, repo, path string) (ApiResponse, error) {
	return c.Transport.Request("GET", path, c.headers(owner, repo, false), nil)
}

// ListIssues returns the open issues carrying label. Filters out pull
// requests -- the issues endpoint returns both, distinguished only by the
// presence of a pull_request key on the item.
func (c *RESTClient) ListIssues(owner, repo, label string) ([]Issue, error) {
	var issues []Issue
	path := fmt.Sprintf("/repos/%s/%s/issues?labels=%s&state=open&per_page=100",
		owner, repo, url.QueryEscape(label))
	for path != "" {
		resp, err := c.get(owner, repo, path)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, &Error{Status: resp.Status, Body: resp.Body}
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(resp.Body, &items); err != nil {
			return nil, err
		}
		for _, raw := range items {
			if _, isPR := raw["pull_request"]; isPR {
				continue
			}
			issue, err := decodeIssue(raw)
			if err != nil {
				return nil, err
			}
			issues = append(issues, issue)
		}
		path = nextPagePath(resp.Headers["Link"])
	}
	return issues, nil
}

type issueJSON struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func decodeIssue(raw map[string]json.RawMessage) (Issue, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return Issue{}, err
	}
	var parsed issueJSON
	if err := json.Unmarshal(data, &parsed); err != nil {
		return Issue{}, err
	}
	names := make([]string, len(parsed.Labels))
	for i, l := range parsed.Labels {
		names[i] = l.Name
	}
	state := parsed.State
	if state == "" {
		state = "open"
	}
	return Issue{
		Number: parsed.Number, Title: parsed.Title, Body: parsed.Body,
		HTMLURL: parsed.HTMLURL, Labels: labelSet(names...), State: state,
		Author: parsed.User.Login,
	}, nil
}

// GetIssue fetches a single issue fresh -- used to read an issue's
// current title when it isn't otherwise on hand (e.g. PR creation, which
// only carries the issue number, not its title).
func (c *RESTClient) GetIssue(owner, repo string, number int) (Issue, error) {
	resp, err := c.get(owner, repo, fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number))
	if err != nil {
		return Issue{}, err
	}
	if resp.Status != 200 {
		return Issue{}, &Error{Status: resp.Status, Body: resp.Body}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return Issue{}, err
	}
	return decodeIssue(raw)
}

func (c *RESTClient) AddLabel(owner, repo string, number int, label string) error {
	body, _ := json.Marshal(map[string][]string{"labels": {label}})
	resp, err := c.Transport.Request(
		"POST", fmt.Sprintf("/repos/%s/%s/issues/%d/labels", owner, repo, number),
		c.headers(owner, repo, true), body,
	)
	if err != nil {
		return err
	}
	if resp.Status != 200 && resp.Status != 201 {
		return &Error{Status: resp.Status, Body: resp.Body}
	}
	return nil
}

// quoteKeepSlash percent-encodes s the way Python's urllib.parse.quote
// does with its default safe="/": every reserved character except "/"
// itself. url.PathEscape encodes "/" too (it's meant for a single
// segment), so this escapes segment-by-segment instead.
func quoteKeepSlash(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// quoteAll percent-encodes s the way Python's urllib.parse.quote(s,
// safe="") does: every byte except the unreserved set (ASCII letters,
// digits, and "_.-~") is escaped -- including "/" and ":", which
// url.PathEscape leaves alone since it allows both within a single path
// segment per RFC 3986. Needed wherever a call builds a qualified
// owner:branch value, since a literal ":" there would otherwise reach
// GitHub unescaped.
func quoteAll(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

func (c *RESTClient) RemoveLabel(owner, repo string, number int, label string) error {
	resp, err := c.Transport.Request(
		"DELETE", fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s", owner, repo, number, quoteKeepSlash(label)),
		c.headers(owner, repo, false), nil,
	)
	if err != nil {
		return err
	}
	// 404 means the label is already off the issue -- a fine outcome, not
	// an error, since the caller's intent ("this label should not be on
	// there") is already satisfied.
	if resp.Status != 200 && resp.Status != 404 {
		return &Error{Status: resp.Status, Body: resp.Body}
	}
	return nil
}

func (c *RESTClient) setIssueState(owner, repo string, number int, state string) error {
	body, _ := json.Marshal(map[string]string{"state": state})
	resp, err := c.Transport.Request(
		"PATCH", fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number),
		c.headers(owner, repo, true), body,
	)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return &Error{Status: resp.Status, Body: resp.Body}
	}
	return nil
}

// CloseIssue closes a task issue directly. A fully qualified "Closes
// owner/repo#N" in a PR body only auto-closes within the *same* repo it's
// opened in -- across repos (the task/target split's normal case) it just
// links, never closes -- so a caller closes the task issue itself rather
// than relying on the PR body text to do it.
func (c *RESTClient) CloseIssue(owner, repo string, number int) error {
	return c.setIssueState(owner, repo, number, "closed")
}

// ReopenIssue reopens a task issue GitHub had closed -- a human reviewing
// "done" work may come back with a follow-up comment instead of
// relabelling it, and this is the other half of CloseIssue, just the
// other state value.
// UpdateIssue edits an issue's title and/or body in place, leaving
// anything else about it (labels, state) untouched -- a caller wanting
// to close or relabel too makes those calls separately. Both nil is the
// caller's own mistake to avoid; this method sends whichever of the two
// it is given, since GitHub's PATCH endpoint leaves an omitted field as
// it was.
func (c *RESTClient) UpdateIssue(owner, repo string, number int, title, body *string) error {
	fields := map[string]string{}
	if title != nil {
		fields["title"] = *title
	}
	if body != nil {
		fields["body"] = *body
	}
	data, _ := json.Marshal(fields)
	resp, err := c.Transport.Request(
		"PATCH", fmt.Sprintf("/repos/%s/%s/issues/%d", owner, repo, number),
		c.headers(owner, repo, true), data,
	)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return &Error{Status: resp.Status, Body: resp.Body}
	}
	return nil
}

func (c *RESTClient) ReopenIssue(owner, repo string, number int) error {
	return c.setIssueState(owner, repo, number, "open")
}

func (c *RESTClient) getBranch(owner, repo, branch string) (ApiResponse, error) {
	// GitHub's branch-get endpoint requires "/" within the branch name
	// itself percent-encoded (%2F), not left as a path separator --
	// quoteAll is what does that.
	return c.get(owner, repo, fmt.Sprintf("/repos/%s/%s/branches/%s", owner, repo, quoteAll(branch)))
}

// BranchExists reports whether branch is really on the remote.
//
// The design decision behind why this exists at all: a dispatch tells the
// agent exactly what branch to push to, but the prompt it received came
// from untrusted issue content, so the caller confirms the branch is real
// via the API before opening a PR against it rather than trusting the
// agent's own report of what it did.
func (c *RESTClient) BranchExists(owner, repo, branch string) (bool, error) {
	resp, err := c.getBranch(owner, repo, branch)
	if err != nil {
		return false, err
	}
	if resp.Status == 200 {
		return true, nil
	}
	if resp.Status == 404 {
		return false, nil
	}
	return false, &Error{Status: resp.Status, Body: resp.Body}
}

// GetBranchHead returns branch's tip commit, or nil if the branch doesn't
// exist -- the same GET BranchExists makes, kept as its own method rather
// than folded into it since most callers only ever need the boolean and
// this one's response also carries the head commit's own message, which
// only a fresh-PR path needs, to seed a real description instead of a
// generic one.
func (c *RESTClient) GetBranchHead(owner, repo, branch string) (*BranchHead, error) {
	resp, err := c.getBranch(owner, repo, branch)
	if err != nil {
		return nil, err
	}
	if resp.Status == 404 {
		return nil, nil
	}
	if resp.Status != 200 {
		return nil, &Error{Status: resp.Status, Body: resp.Body}
	}
	var data struct {
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return nil, err
	}
	return &BranchHead{SHA: data.Commit.SHA, Message: data.Commit.Commit.Message}, nil
}

// CreateBranch creates branch pointing at sha -- release management's own
// "cut" (bwsalmon/agents#398): a fresh, never-before-existing ref, via
// GitHub's git-database API rather than the higher-level "create from" a
// human uses on github.com, since the caller already has the exact
// commit in hand (typically another branch's own GetBranchHead) and has
// no reason to ask GitHub to resolve one itself. GitHub answers 422 if
// branch already exists; that reaches the caller as an *Error the same
// as any other non-201 status, since a caller wanting "create or move"
// has UpdateBranch for the second half.
func (c *RESTClient) CreateBranch(owner, repo, branch, sha string) error {
	payload, _ := json.Marshal(map[string]string{"ref": "refs/heads/" + branch, "sha": sha})
	resp, err := c.Transport.Request(
		"POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo),
		c.headers(owner, repo, true), payload,
	)
	if err != nil {
		return err
	}
	if resp.Status != 201 {
		return &Error{Status: resp.Status, Body: resp.Body}
	}
	return nil
}

// UpdateBranch moves branch's tip to sha -- release management's other
// half of a cut (repointing the repo's own moving rc branch at a freshly
// cut candidate) and of a promotion (fast-forwarding ProdBranch to the
// candidate being promoted). force matches GitHub's own field: false
// refuses a move that is not a fast-forward, true moves it regardless --
// a caller doing the former should treat a refusal (422) as a caller
// error, not a transient failure to retry unmodified.
func (c *RESTClient) UpdateBranch(owner, repo, branch, sha string, force bool) error {
	payload, _ := json.Marshal(map[string]any{"sha": sha, "force": force})
	resp, err := c.Transport.Request(
		"PATCH", fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", owner, repo, branch),
		c.headers(owner, repo, true), payload,
	)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return &Error{Status: resp.Status, Body: resp.Body}
	}
	return nil
}

func (c *RESTClient) CreatePullRequest(owner, repo, head, base, title, body string) (PullRequest, error) {
	payload, _ := json.Marshal(map[string]string{
		"title": title, "head": head, "base": base, "body": body,
	})
	resp, err := c.Transport.Request(
		"POST", fmt.Sprintf("/repos/%s/%s/pulls", owner, repo),
		c.headers(owner, repo, true), payload,
	)
	if err != nil {
		return PullRequest{}, err
	}
	if resp.Status != 201 {
		return PullRequest{}, &Error{Status: resp.Status, Body: resp.Body}
	}
	var data struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return PullRequest{}, err
	}
	return PullRequest{Number: data.Number, HTMLURL: data.HTMLURL}, nil
}

// FindOpenPullRequestForBranch returns the open PR whose head is branch,
// if there is one.
//
// GitHub allows at most one open PR per head branch, so this is either
// exactly one or none -- used to recognise "the PR for this task already
// exists" after a CreatePullRequest 422, rather than failing a finish
// that has in fact already succeeded.
//
// head is qualified owner:branch the way GitHub's own filter requires;
// grain only ever pushes to branches in the target repo itself (never a
// fork), so the head owner is always owner.
func (c *RESTClient) FindOpenPullRequestForBranch(owner, repo, branch string) (*PullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&head=%s",
		owner, repo, quoteAll(owner+":"+branch))
	resp, err := c.get(owner, repo, path)
	if err != nil {
		return nil, err
	}
	if resp.Status != 200 {
		return nil, &Error{Status: resp.Status, Body: resp.Body}
	}
	var items []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(resp.Body, &items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	return &PullRequest{Number: items[0].Number, HTMLURL: items[0].HTMLURL}, nil
}

// CreateIssue files a new issue on the task repo. labels lets it land
// already carrying a needs-approval label in the same request, rather
// than a second AddLabel call that could fail partway and leave the issue
// unlabelled.
func (c *RESTClient) CreateIssue(owner, repo, title, body string, labels []string) (Issue, error) {
	payload := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		payload["labels"] = labels
	}
	data, _ := json.Marshal(payload)
	resp, err := c.Transport.Request(
		"POST", fmt.Sprintf("/repos/%s/%s/issues", owner, repo),
		c.headers(owner, repo, true), data,
	)
	if err != nil {
		return Issue{}, err
	}
	if resp.Status != 201 {
		return Issue{}, &Error{Status: resp.Status, Body: resp.Body}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return Issue{}, err
	}
	return decodeIssue(raw)
}

// MergePullRequest merges a PR directly, rather than leaving it for a
// human to click. A 405 (not mergeable -- GitHub's own answer if
// Mergeable went stale between the read and this call) or 409 (base
// branch moved underneath it, or headSHA no longer matches) is left for
// the caller to decide whether to retry next cycle via the returned
// *Error's Status; only a genuinely unexpected status is a surprise here.
//
// headSHA, when non-empty, goes in the body as GitHub's own `sha` field
// -- "SHA that pull request head must match to allow merge" -- and makes
// this a merge of one named commit rather than of whatever the head
// branch points at by the time the request lands. A caller that decided
// to merge by reading a commit's CI wants exactly that: without it, a
// push landing in the gap between the read and this call is merged
// untested, and no answer the caller got back could have told it so.
// GitHub answers 409 when the branch has moved, which is the caller's cue
// to look again at the commit that is there now.
//
// Empty means unpinned, the old behaviour: for a caller merging on a
// human's say-so rather than on a verdict it computed itself, there is no
// commit the merge has to match, and pinning one would only refuse a
// merge the human asked for.
func (c *RESTClient) MergePullRequest(owner, repo string, number int, headSHA string) error {
	payload := map[string]any{}
	if headSHA != "" {
		payload["sha"] = headSHA
	}
	data, _ := json.Marshal(payload)
	resp, err := c.Transport.Request(
		"PUT", fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, number),
		c.headers(owner, repo, true), data,
	)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return &Error{Status: resp.Status, Body: resp.Body}
	}
	return nil
}

func (c *RESTClient) GetPullRequest(owner, repo string, number int) (PullRequestDetail, error) {
	resp, err := c.get(owner, repo, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number))
	if err != nil {
		return PullRequestDetail{}, err
	}
	if resp.Status != 200 {
		return PullRequestDetail{}, &Error{Status: resp.Status, Body: resp.Body}
	}
	var data struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Head    struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Mergeable      *bool      `json:"mergeable"`
		MergeableState string     `json:"mergeable_state"`
		Merged         bool       `json:"merged"`
		MergedAt       *time.Time `json:"merged_at"`
		CreatedAt      time.Time  `json:"created_at"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return PullRequestDetail{}, err
	}
	state := data.State
	if state == "" {
		state = "open"
	}
	return PullRequestDetail{
		Number: data.Number, Title: data.Title, Body: data.Body, HTMLURL: data.HTMLURL,
		HeadRef: data.Head.Ref, HeadSHA: data.Head.SHA, BaseRef: data.Base.Ref,
		State: state, Mergeable: data.Mergeable, MergeableState: data.MergeableState,
		Merged: data.Merged, MergedAt: data.MergedAt, CreatedAt: data.CreatedAt,
	}, nil
}

// DefaultBranch is the target repo's own default branch -- the base a PR
// opened there should target when a task's own directive doesn't say
// otherwise.
//
// Read from GitHub rather than configured: with one repo per deployment a
// single configured base branch was a fair guess, but a task repo
// dispatching into many target repos would need an operator to keep a
// per-repo table of "main" vs "master" vs "trunk" correct, and GitHub
// already knows.
func (c *RESTClient) DefaultBranch(owner, repo string) (string, error) {
	resp, err := c.get(owner, repo, fmt.Sprintf("/repos/%s/%s", owner, repo))
	if err != nil {
		return "", err
	}
	if resp.Status != 200 {
		return "", &Error{Status: resp.Status, Body: resp.Body}
	}
	var data struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return "", err
	}
	return data.DefaultBranch, nil
}

// ListReviewComments returns the inline review comments on a PR -- the
// context a dispatch needs to tell the agent what feedback it's
// addressing. Paginates the same way ListIssues does; GitHub's REST API
// paginates every list endpoint via the same Link header convention, not
// something specific to the issues endpoint.
func (c *RESTClient) ListReviewComments(owner, repo string, number int) ([]ReviewComment, error) {
	var comments []ReviewComment
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", owner, repo, number)
	for path != "" {
		resp, err := c.get(owner, repo, path)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, &Error{Status: resp.Status, Body: resp.Body}
		}
		var items []struct {
			ID   int    `json:"id"`
			Body string `json:"body"`
			Path string `json:"path"`
			Line *int   `json:"line"`
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(resp.Body, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			comments = append(comments, ReviewComment{
				ID: item.ID, User: item.User.Login, Body: item.Body,
				Path: item.Path, Line: item.Line,
			})
		}
		path = nextPagePath(resp.Headers["Link"])
	}
	return comments, nil
}

// ListCheckRuns returns the check runs against ref -- a branch name works
// fine here, GitHub resolves it to that branch's current tip itself, so a
// caller never needs a commit sha in hand. Paginates the same Link-header
// way every other list endpoint here does, but the response body itself
// is shaped differently -- {"total_count": N, "check_runs": [...]}, not a
// bare array, which is GitHub's own shape for this one endpoint.
func (c *RESTClient) ListCheckRuns(owner, repo, ref string) ([]CheckRun, error) {
	var runs []CheckRun
	path := fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, repo, url.PathEscape(ref))
	for path != "" {
		resp, err := c.get(owner, repo, path)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, &Error{Status: resp.Status, Body: resp.Body}
		}
		var data struct {
			CheckRuns []struct {
				Name       string  `json:"name"`
				Status     string  `json:"status"`
				Conclusion *string `json:"conclusion"`
			} `json:"check_runs"`
		}
		if err := json.Unmarshal(resp.Body, &data); err != nil {
			return nil, err
		}
		for _, item := range data.CheckRuns {
			runs = append(runs, CheckRun{Name: item.Name, Status: item.Status, Conclusion: item.Conclusion})
		}
		path = nextPagePath(resp.Headers["Link"])
	}
	return runs, nil
}

// ListWorkflowRuns returns the GitHub Actions workflow runs against one
// commit, shaped as CheckRuns so a caller can treat them exactly like the
// check runs ListCheckRuns returns.
//
// This is the fallback for a credential that cannot reach the Checks API.
// Reading check runs needs the `repo` scope on a classic PAT, and on a
// fine-grained PAT it cannot be granted at all -- GitHub offered a
// "Checks" permission for fine-grained tokens, withdrew it over "some
// edge cases", and has said only GitHub Apps may use that API in the
// meantime. This endpoint sits behind the "Actions" repository
// permission instead, which a fine-grained token *can* hold, so between
// the two every credential type a deployment might use has some way to
// read CI.
//
// What it costs is generality, not accuracy: Actions runs are all this
// sees, so CI reported through the Checks API by a third party
// (Buildkite, CircleCI, a review bot) is invisible here where
// ListCheckRuns would have shown it. A deployment whose checks are
// GitHub Actions loses nothing; one that adds another CI provider needs
// a credential that can read checks properly.
//
// headSHA is a commit sha, not a branch: filtering by branch would
// return runs from older commits too, and the newest run for a workflow
// on a branch is not necessarily a run of the commit the PR now points
// at. Reading a stale pass as this commit's pass is the one error that
// would auto-merge untested code.
//
// The response is {"total_count": N, "workflow_runs": [...]}, the same
// wrapped shape the Checks API uses rather than a bare array.
func (c *RESTClient) ListWorkflowRuns(owner, repo, headSHA string) ([]CheckRun, error) {
	var runs []CheckRun
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?head_sha=%s&per_page=100",
		owner, repo, url.QueryEscape(headSHA))
	for path != "" {
		resp, err := c.get(owner, repo, path)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, &Error{Status: resp.Status, Body: resp.Body}
		}
		var data struct {
			WorkflowRuns []struct {
				Name       string  `json:"name"`
				Status     string  `json:"status"`
				Conclusion *string `json:"conclusion"`
			} `json:"workflow_runs"`
		}
		if err := json.Unmarshal(resp.Body, &data); err != nil {
			return nil, err
		}
		for _, item := range data.WorkflowRuns {
			runs = append(runs, CheckRun{
				Name: item.Name, Status: item.Status, Conclusion: item.Conclusion,
			})
		}
		path = nextPagePath(resp.Headers["Link"])
	}
	return runs, nil
}

// FailedJobLogs returns the GitHub Actions jobs that failed against one
// commit, each with the tail of its own log.
//
// Three reads, because GitHub has no endpoint from a commit to a log:
// the runs for the commit, the jobs of each run that failed, and then
// each failed job's log. It goes through Actions rather than the Checks
// API for the same reason ListWorkflowRuns does -- Checks cannot be
// granted to a fine-grained PAT at all, while "Actions" read can, and a
// classic PAT's `repo` scope covers both. What that costs is the same
// thing it costs there: CI reported by a third party (Buildkite,
// CircleCI) has no Actions job behind it and so no log here. The caller
// gets an empty result, not an error, and a fix task's body says what it
// always said.
//
// The logs endpoint answers 302 to a short-lived storage URL, which the
// http.Client under RealTransport follows on its own (dropping the
// Authorization header on the cross-host hop, as it should). A job whose
// log has expired -- GitHub keeps them 90 days by default -- answers 410
// instead, and a job whose logs this credential cannot read answers 403;
// both are skipped rather than failing the call, since the log is the
// bonus here and the job names are the part the caller already has.
//
// headSHA may be empty on a pull request read before GitHub filled it in,
// the same way it may be for checkRunsFor's fallback; there is nothing to
// scope to, so this answers nothing.
func (c *RESTClient) FailedJobLogs(owner, repo, headSHA string) ([]JobLog, error) {
	if headSHA == "" {
		return nil, nil
	}
	runIDs, err := c.failedRunIDs(owner, repo, headSHA)
	if err != nil {
		return nil, err
	}
	var logs []JobLog
	for _, runID := range runIDs {
		jobs, err := c.failedJobs(owner, repo, runID)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			if len(logs) >= maxFailedJobLogs {
				return logs, nil
			}
			text, err := c.jobLog(owner, repo, job.ID)
			if err != nil {
				return nil, err
			}
			if text == "" {
				continue
			}
			tail, truncated := tailOf(text, JobLogTailBytes)
			logs = append(logs, JobLog{
				Name: job.Name, URL: job.URL, Log: tail, Truncated: truncated,
			})
		}
	}
	return logs, nil
}

// failedRunIDs is FailedJobLogs' first read: the workflow runs against
// headSHA that finished and did not pass. Scoped to the commit, not the
// branch, for the reason ListWorkflowRuns' own doc comment gives.
func (c *RESTClient) failedRunIDs(owner, repo, headSHA string) ([]int64, error) {
	var ids []int64
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?head_sha=%s&per_page=100",
		owner, repo, url.QueryEscape(headSHA))
	for path != "" {
		resp, err := c.get(owner, repo, path)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, &Error{Status: resp.Status, Body: resp.Body}
		}
		var data struct {
			WorkflowRuns []struct {
				ID         int64  `json:"id"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"workflow_runs"`
		}
		if err := json.Unmarshal(resp.Body, &data); err != nil {
			return nil, err
		}
		for _, run := range data.WorkflowRuns {
			if run.Status == "completed" && failedConclusion(run.Conclusion) {
				ids = append(ids, run.ID)
			}
		}
		path = nextPagePath(resp.Headers["Link"])
	}
	return ids, nil
}

// jobRef is one job of a workflow run, as much of it as fetching a log
// and naming it afterwards needs.
type jobRef struct {
	ID   int64
	Name string
	URL  string
}

// failedJobs is FailedJobLogs' second read: the jobs of one run that did
// not pass. A run fails as a whole, but its log is per job -- and it is
// the job's name ("go", "ui e2e") that matches what a check run is
// called, so this is also where the two views of the same CI line up.
func (c *RESTClient) failedJobs(owner, repo string, runID int64) ([]jobRef, error) {
	var jobs []jobRef
	path := fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=100", owner, repo, runID)
	for path != "" {
		resp, err := c.get(owner, repo, path)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, &Error{Status: resp.Status, Body: resp.Body}
		}
		var data struct {
			Jobs []struct {
				ID         int64  `json:"id"`
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				HTMLURL    string `json:"html_url"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(resp.Body, &data); err != nil {
			return nil, err
		}
		for _, job := range data.Jobs {
			if job.Status == "completed" && failedConclusion(job.Conclusion) {
				jobs = append(jobs, jobRef{ID: job.ID, Name: job.Name, URL: job.HTMLURL})
			}
		}
		path = nextPagePath(resp.Headers["Link"])
	}
	return jobs, nil
}

// jobLog is FailedJobLogs' third read: one job's whole log as plain
// text, or "" when GitHub will not serve it (see FailedJobLogs on the
// 410 and 403 cases). Only a transport-level failure is an error --
// nothing above this can carry on regardless of what one job's log says.
func (c *RESTClient) jobLog(owner, repo string, jobID int64) (string, error) {
	resp, err := c.get(owner, repo, fmt.Sprintf("/repos/%s/%s/actions/jobs/%d/logs", owner, repo, jobID))
	if err != nil {
		return "", err
	}
	if resp.Status != 200 {
		return "", nil
	}
	return string(resp.Body), nil
}

// tailOf returns the last limit bytes of text, advanced to the next line
// boundary so it never opens mid-line, and whether anything was cut.
func tailOf(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	tail := text[len(text)-limit:]
	if at := strings.IndexByte(tail, '\n'); at >= 0 {
		tail = tail[at+1:]
	}
	return tail, true
}

// ListComments returns the plain top-level conversation on an issue or PR
// -- where a human's reply to an agent's ask_question call lands. Same
// shared issues-comments endpoint and pagination shape ListReviewComments
// uses for its own (inline) endpoint.
func (c *RESTClient) ListComments(owner, repo string, number int) ([]Comment, error) {
	var comments []Comment
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number)
	for path != "" {
		resp, err := c.get(owner, repo, path)
		if err != nil {
			return nil, err
		}
		if resp.Status != 200 {
			return nil, &Error{Status: resp.Status, Body: resp.Body}
		}
		var items []struct {
			ID                int    `json:"id"`
			Body              string `json:"body"`
			AuthorAssociation string `json:"author_association"`
			User              struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		if err := json.Unmarshal(resp.Body, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			assoc := item.AuthorAssociation
			if assoc == "" {
				assoc = "NONE"
			}
			comments = append(comments, Comment{
				ID: item.ID, User: item.User.Login, Body: item.Body, AuthorAssociation: assoc,
			})
		}
		path = nextPagePath(resp.Headers["Link"])
	}
	return comments, nil
}

// CreateComment posts a top-level comment. Not something the agent can
// reach directly: the only caller relays an ask_question call to a human.
//
// Returns the new comment's id -- recorded as the baseline for "has a
// trusted reply arrived after this," since a comment's own id is the one
// thing that can't be spoofed by editing an earlier comment's body.
func (c *RESTClient) CreateComment(owner, repo string, number int, body string) (int, error) {
	payload, _ := json.Marshal(map[string]string{"body": body})
	resp, err := c.Transport.Request(
		"POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number),
		c.headers(owner, repo, true), payload,
	)
	if err != nil {
		return 0, err
	}
	if resp.Status != 201 {
		return 0, &Error{Status: resp.Status, Body: resp.Body}
	}
	var data struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return 0, err
	}
	return data.ID, nil
}

// CreateReview creates a draft review on a pull request -- see the
// package doc comment for why it is always a draft. Returns the new
// review's id, kept for the same reason CreateComment returns one: cheap,
// and the pattern every other creation call here already follows.
func (c *RESTClient) CreateReview(owner, repo string, number int, body string, comments []NewReviewComment) (int, error) {
	payloadComments := make([]map[string]any, len(comments))
	for i, cm := range comments {
		payloadComments[i] = map[string]any{"path": cm.Path, "line": cm.Line, "body": cm.Body}
	}
	payload, _ := json.Marshal(map[string]any{"body": body, "comments": payloadComments})
	resp, err := c.Transport.Request(
		"POST", fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number),
		c.headers(owner, repo, true), payload,
	)
	if err != nil {
		return 0, err
	}
	if resp.Status != 200 {
		return 0, &Error{Status: resp.Status, Body: resp.Body}
	}
	var data struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return 0, err
	}
	return data.ID, nil
}

// DryRunClient wraps a Client: reads pass through, mutations print
// instead of firing. Same split a dry-run command runner makes for local
// commands -- "read-only commands still execute."
type DryRunClient struct {
	Inner Client
}

func (d DryRunClient) ListIssues(owner, repo, label string) ([]Issue, error) {
	return d.Inner.ListIssues(owner, repo, label)
}

func (d DryRunClient) GetIssue(owner, repo string, number int) (Issue, error) {
	return d.Inner.GetIssue(owner, repo, number)
}

func (d DryRunClient) AddLabel(owner, repo string, number int, label string) error {
	fmt.Printf("+ add label %q to %s/%s#%d\n", label, owner, repo, number)
	return nil
}

func (d DryRunClient) RemoveLabel(owner, repo string, number int, label string) error {
	fmt.Printf("+ remove label %q from %s/%s#%d\n", label, owner, repo, number)
	return nil
}

func (d DryRunClient) CloseIssue(owner, repo string, number int) error {
	fmt.Printf("+ close issue %s/%s#%d\n", owner, repo, number)
	return nil
}

func (d DryRunClient) ReopenIssue(owner, repo string, number int) error {
	fmt.Printf("+ reopen issue %s/%s#%d\n", owner, repo, number)
	return nil
}

func (d DryRunClient) UpdateIssue(owner, repo string, number int, title, body *string) error {
	fmt.Printf("+ update issue %s/%s#%d (title set: %t, body set: %t)\n", owner, repo, number, title != nil, body != nil)
	return nil
}

func (d DryRunClient) BranchExists(owner, repo, branch string) (bool, error) {
	return d.Inner.BranchExists(owner, repo, branch)
}

func (d DryRunClient) GetBranchHead(owner, repo, branch string) (*BranchHead, error) {
	return d.Inner.GetBranchHead(owner, repo, branch)
}

func (d DryRunClient) FindOpenPullRequestForBranch(owner, repo, branch string) (*PullRequest, error) {
	return d.Inner.FindOpenPullRequestForBranch(owner, repo, branch)
}

func (d DryRunClient) CreateBranch(owner, repo, branch, sha string) error {
	fmt.Printf("+ create branch %s/%s:%s at %s\n", owner, repo, branch, sha)
	return nil
}

func (d DryRunClient) UpdateBranch(owner, repo, branch, sha string, force bool) error {
	fmt.Printf("+ move branch %s/%s:%s to %s (force=%t)\n", owner, repo, branch, sha, force)
	return nil
}

func (d DryRunClient) CreatePullRequest(owner, repo, head, base, title, body string) (PullRequest, error) {
	fmt.Printf("+ open PR %s/%s: %q -> %q (%q)\n", owner, repo, head, base, title)
	return PullRequest{Number: 0, HTMLURL: fmt.Sprintf("(dry run) %s/%s: %s -> %s", owner, repo, head, base)}, nil
}

func (d DryRunClient) CreateIssue(owner, repo, title, body string, labels []string) (Issue, error) {
	fmt.Printf("+ file issue %s/%s: %q %v\n", owner, repo, title, labels)
	return Issue{Number: 0, Title: title, Body: body, HTMLURL: fmt.Sprintf("(dry run) %s/%s", owner, repo), Labels: labelSet(labels...)}, nil
}

func (d DryRunClient) MergePullRequest(owner, repo string, number int, headSHA string) error {
	if headSHA != "" {
		fmt.Printf("+ merge PR %s/%s#%d (only if its head is still %s)\n", owner, repo, number, headSHA)
		return nil
	}
	fmt.Printf("+ merge PR %s/%s#%d\n", owner, repo, number)
	return nil
}

func (d DryRunClient) GetPullRequest(owner, repo string, number int) (PullRequestDetail, error) {
	return d.Inner.GetPullRequest(owner, repo, number)
}

func (d DryRunClient) DefaultBranch(owner, repo string) (string, error) {
	return d.Inner.DefaultBranch(owner, repo)
}

func (d DryRunClient) ListReviewComments(owner, repo string, number int) ([]ReviewComment, error) {
	return d.Inner.ListReviewComments(owner, repo, number)
}

func (d DryRunClient) ListCheckRuns(owner, repo, ref string) ([]CheckRun, error) {
	return d.Inner.ListCheckRuns(owner, repo, ref)
}

func (d DryRunClient) ListWorkflowRuns(owner, repo, headSHA string) ([]CheckRun, error) {
	return d.Inner.ListWorkflowRuns(owner, repo, headSHA)
}

func (d DryRunClient) FailedJobLogs(owner, repo, headSHA string) ([]JobLog, error) {
	return d.Inner.FailedJobLogs(owner, repo, headSHA)
}

func (d DryRunClient) ListComments(owner, repo string, number int) ([]Comment, error) {
	return d.Inner.ListComments(owner, repo, number)
}

func (d DryRunClient) CreateComment(owner, repo string, number int, body string) (int, error) {
	fmt.Printf("+ comment on %s/%s#%d: %q\n", owner, repo, number, body)
	return 0, nil
}

func (d DryRunClient) CreateReview(owner, repo string, number int, body string, comments []NewReviewComment) (int, error) {
	fmt.Printf("+ draft review on %s/%s#%d: %q (%d inline comment(s))\n", owner, repo, number, body, len(comments))
	return 0, nil
}

var _ Client = (*RESTClient)(nil)
var _ Client = DryRunClient{}
