// Package githubsim replicates v1's own live-test double for the GitHub
// REST API -- tests/test_live_issue_to_pr.py's RealGitHubMock -- as a
// github.Transport, so a Go end-to-end test can wire a real
// github.RESTClient against it exactly the way github.FakeTransport lets
// a unit test wire one against a single scripted response. The
// difference is state: Sim holds a mutable set of issues and pull
// requests across a whole test, so a sequence of calls (add a label, list
// issues, open a PR, list pull requests again) sees the effect of the one
// before it, the same as talking to real GitHub across a real
// orchestration run would.
//
// Sim implements every endpoint github.Client's real implementation
// (github.RESTClient) calls: list/get/create issues, close/reopen/update
// an issue, add/remove a label, default branch, branch existence,
// create/find/get/merge a pull request, merging one branch into another,
// the plain comment thread, inline review comments and draft reviews, and
// check runs -- the whole surface
// github.go's Client interface names, so a live end-to-end test never
// has to fall back to github.FakeTransport partway through for an
// endpoint Sim doesn't yet answer. Wired in as a github.Transport,
// github.RESTClient's own logic (path building, pagination via the shape
// of a response's own Link header where it applies, status-code
// handling, JSON field extraction) runs completely unmodified; only the
// network call underneath is swapped.
//
// BranchExists is the one endpoint that would be dishonest as a canned
// answer: whether a branch is "real" is exactly the question a live
// end-to-end test is trying to prove an agent's push settled, so Sim
// answers it by running a real `git show-ref` against a real bare repo
// rather than consulting its own bookkeeping.
package githubsim

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
)

var (
	issueCommentsRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)/comments$`)
	issueRe         = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)$`)
	// repoRe: the target repo's own default branch, read once per
	// dispatch so the PR base comes from the repo itself rather than a
	// single configured base branch.
	repoRe         = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)$`)
	issuesRe       = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues$`)
	labelsPostRe   = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)/labels$`)
	labelDeleteRe  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)/labels/([^/]+)$`)
	branchRe       = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/branches/(.+)$`)
	pullsRe        = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls$`)
	pullRe         = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/(\d+)$`)
	pullMergeRe    = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/(\d+)/merge$`)
	pullCommentsRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/(\d+)/comments$`)
	pullReviewsRe  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls/(\d+)/reviews$`)
	checkRunsRe    = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/commits/([^/]+)/check-runs$`)
	// The three Actions endpoints github.RESTClient.FailedJobLogs walks
	// from a commit to a log: the commit's runs, one run's jobs, one
	// job's log. actionsRunsRe is also what ListWorkflowRuns reads when a
	// credential cannot reach the Checks API.
	actionsRunsRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/runs$`)
	runJobsRe     = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/runs/(\d+)/jobs$`)
	jobLogsRe     = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/actions/jobs/(\d+)/logs$`)
	// gitRefsRe/gitRefRe: the git-database endpoints CreateBranch and
	// UpdateBranch call -- release management's own (bwsalmon/agents#398).
	gitRefsRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/git/refs$`)
	gitRefRe  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/git/refs/heads/(.+)$`)
	// mergesRe: the branch-into-branch merge the merge queue makes to
	// bring a stale head up to date with its base
	// (github.RESTClient.MergeBranch, orchestrator.refreshStaleHead).
	mergesRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/merges$`)
)

// Issue is one seeded or created fake issue -- Sim's own bookkeeping, not
// github.Issue, since Sim needs a mutable Labels set an incoming request
// can add to or remove from.
type Issue struct {
	Title  string
	Body   string
	Author string
	Labels map[string]struct{}
	// State is GitHub's own open/closed field -- defaults to the zero
	// value, which issueJSON below treats as "open", the same fallback
	// github.decodeIssue applies to a real response with no state field.
	State string
}

// Review is one draft review Sim recorded through CreateReview -- kept
// separately from ReviewComments (below), the same way real GitHub keeps
// a pending review's own comments invisible through the review-comments
// endpoint until a human submits it on github.com (github.go's own
// package doc comment on why create_review only ever posts a draft).
type Review struct {
	Number   int
	Body     string
	Comments []github.NewReviewComment
}

// PullRequest is one pull request Sim recorded through CreatePullRequest.
type PullRequest struct {
	Number  int
	Title   string
	Body    string
	Head    string
	Base    string
	HTMLURL string
	// State is GitHub's own open/closed field. Defaults to "open" at
	// creation; MergePullRequest (below) sets it to "closed" the same way
	// a real merge does, and a test can set it directly to stand in for
	// GitHub's own merge button the way e2e's mergeBranchIntoDefault
	// does for the git side of a merge.
	State string
	// Mergeable stands in for GitHub's own asynchronously computed field
	// -- nil (unknown) unless a test sets it, since Sim has no real merge
	// engine to compute it with.
	Mergeable *bool
	// Merged and MergedAt are GitHub's own fields for the one thing State
	// alone cannot say: whether this PR closed by merging rather than
	// without merging (github.PullRequestDetail.Merged's own doc
	// comment). MergePullRequest sets both; a test standing in for a
	// human closing a PR without merging (setting State to "closed"
	// directly, the same doc comment above already describes) leaves
	// Merged false, matching GitHub's own field for that case.
	Merged   bool
	MergedAt time.Time
	// CreatedAt is GitHub's own field: when the PR was opened, set by
	// CreatePullRequest -- Sim's real wall clock rather than a caller's
	// injected one, since Sim has no clock of its own to inject
	// (unlike orchestrator.SyncPullRequests, which takes now from its
	// caller).
	CreatedAt time.Time
}

// Call is one request Sim answered, kept for tests that want to assert on
// what was actually asked of it.
type Call struct {
	Method string
	Path   string
}

// WorkflowJob is one GitHub Actions job of one commit's run, and what it
// printed -- the thing the merge queue reads to put a failing build's own
// output into the fix task it files (orchestrator.failingJobLogs).
//
// There is no Status field: a seeded job is one that has run, so Sim
// answers "completed" for all of them. An empty Conclusion reads as
// "success", so a test seeding a job it wants ignored need only leave it
// blank.
type WorkflowJob struct {
	Name       string
	Conclusion string
	// Log is what the logs endpoint serves for this job. Empty means
	// GitHub has none to serve -- answered 410, the way a real one
	// answers for a log past its retention window, which is a case
	// FailedJobLogs has to skip rather than fail on.
	Log string
}

// Sim is a github.Transport backed by an in-memory set of issues and pull
// requests, plus a real bare git repo for the one endpoint
// (BranchExists/branch lookups) that would be dishonest as a canned
// answer. Owner and Repo are the only repo Sim answers for -- a request
// naming any other repo is an unhandled-request panic, the same as v1's
// own mock, since a test wiring a second repo in in error should fail
// loudly rather than silently return an empty answer.
type Sim struct {
	Owner         string
	Repo          string
	BareRepo      string
	DefaultBranch string

	Issues       map[int]*Issue
	PullRequests []PullRequest
	Calls        []Call

	// Comments is every posted top-level comment, by issue/PR number --
	// GitHub's own issues-comments endpoint serves both, per Comment's own
	// doc comment in github.go.
	Comments map[int][]github.Comment
	// CheckRuns is keyed by whatever ref a test seeds it under -- a branch
	// name or a sha. A read for either finds it: BareRepo is a real
	// repository, so the branch a test named and the sha the caller asks
	// by resolve to one commit here the same way they do on GitHub (see
	// checkRunsFor).
	CheckRuns map[string][]github.CheckRun
	// WorkflowJobs is the CI behind those check runs: the GitHub Actions
	// jobs, and their logs, that github.RESTClient.FailedJobLogs reaches
	// through a commit's workflow runs. Keyed like CheckRuns, except that
	// a branch name works even though the endpoints are called with a sha
	// -- see workflowJobsFor, which resolves one to the other against the
	// bare repo, since a test seeding these has no way to know a sha it
	// never wrote down.
	//
	// Seeded, not derived: a Sim that invented a job per seeded check run
	// would decide for the test what CI looked like, and the point of
	// these is a test saying what one particular job printed.
	WorkflowJobs map[string][]WorkflowJob
	// ReviewComments is every inline review comment on a PR, keyed by PR
	// number -- what ListReviewComments reads back. Seeded directly by a
	// test standing in for a human reviewer's own comments, since
	// CreateReview's own draft reviews never land here (see Review's doc
	// comment above).
	ReviewComments map[int][]github.ReviewComment
	// Reviews is every draft review CreateReview has recorded, in call
	// order -- Sim's own bookkeeping for a test to assert what an
	// add_review_comment call produced, the same append-only shape
	// PullRequests already uses.
	Reviews []Review

	nextCommentID int
	nextIssue     int
	nextReviewID  int

	// runKeys is the commits (or branch names -- see WorkflowJobs) Sim
	// has answered a workflow-run id for, in the order it first did.
	// Real GitHub's run ids are its own; here the index in this slice is
	// the id, so that the runs endpoint and the jobs endpoint after it
	// agree about which commit is being asked about without Sim having to
	// keep a second map to invert.
	runKeys []string

	// mu serialises Request, which is every mutation this double makes:
	// the orchestrator dispatches concurrently -- a goroutine per run,
	// and since a run now outlives the cycle that started it (see
	// orchestrator.InFlight), several of them at once against one client
	// -- so two runs really do reach a Sim at the same time. Nothing
	// about the maps and slices above is safe for that on its own, and a
	// double that corrupts its own state under the very concurrency the
	// code it stands in for is built for would fail tests for a reason
	// that has nothing to do with what they assert.
	//
	// One lock for the whole method, rather than something finer: a
	// request here is microseconds of map work plus the occasional git
	// command against BareRepo, which has to be serialised anyway.
	// Fields are still read directly by tests, which do that before or
	// after a cycle rather than during one.
	mu sync.Mutex
}

// New returns a Sim seeded with no issues and no pull requests, answering
// for owner/repo, with defaultBranch as the target repo's own default
// branch and bareRepo as the real git repository BranchExists checks
// against.
func New(owner, repo, bareRepo, defaultBranch string) *Sim {
	return &Sim{
		Owner: owner, Repo: repo, BareRepo: bareRepo, DefaultBranch: defaultBranch,
		Issues:         map[int]*Issue{},
		Comments:       map[int][]github.Comment{},
		CheckRuns:      map[string][]github.CheckRun{},
		WorkflowJobs:   map[string][]WorkflowJob{},
		ReviewComments: map[int][]github.ReviewComment{},
		nextCommentID:  1000,
		nextIssue:      8000,
		nextReviewID:   5000,
	}
}

func (s *Sim) branchExists(branch string) bool {
	cmd := exec.Command("git", "--git-dir", s.BareRepo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

// branchHead resolves branch's own tip commit for real, against BareRepo
// -- what backs GetBranchHead the same honest way branchExists backs
// BranchExists (this file's own doc comment): a caller cutting a release
// candidate from it needs the real sha CreateBranch then pins, not a
// canned one a test would have to keep in sync with whatever pushBranch
// last committed.
func (s *Sim) branchHead(branch string) (sha, message string, ok bool) {
	if !s.branchExists(branch) {
		return "", "", false
	}
	shaOut, err := exec.Command("git", "--git-dir", s.BareRepo, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		return "", "", false
	}
	msgOut, err := exec.Command("git", "--git-dir", s.BareRepo, "log", "-1", "--format=%B", "refs/heads/"+branch).Output()
	if err != nil {
		return "", "", false
	}
	return strings.TrimSpace(string(shaOut)), strings.TrimRight(string(msgOut), "\n"), true
}

// branchSHA is branchHead's first half on its own: branch's tip, or ""
// if there is no such branch. Separate because the pull-request detail
// endpoint needs the sha on every read and nothing else -- branchHead
// would fork git three times (an existence check, rev-parse, and a log
// for a commit message with no reader) where `rev-parse --verify` on its
// own answers both questions in one, and a load test reading a pull
// request in a loop pays that difference.
func (s *Sim) branchSHA(branch string) string {
	out, err := exec.Command("git", "--git-dir", s.BareRepo,
		"rev-parse", "-q", "--verify", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// setBranch points branch at sha -- update-ref both creates a ref that
// doesn't yet exist and moves one that does, which is what lets Sim
// answer CreateBranch and UpdateBranch with the same underlying command;
// real GitHub's own git-database API is the two different endpoints
// github.go's own doc comments on those methods explain.
func (s *Sim) setBranch(branch, sha string) error {
	return runGit(s.BareRepo, "git", "--git-dir", s.BareRepo, "update-ref", "refs/heads/"+branch, sha)
}

// mergeIntoBase performs pr's merge for real, at the git level, against
// BareRepo -- real GitHub's own PUT .../merge endpoint moves commits into
// the base branch as a side effect, not just github.PullRequestDetail's
// State; a Sim that only flipped State to "closed" would let a caller
// believe a merge landed when the base branch never actually moved, which
// is exactly the gap an end-to-end test asserting on the base branch's
// own git log (e2e's own harness already does this for a human's
// merge via mergeBranchIntoDefault) would otherwise never catch for the
// auto-merge path, where nothing else performs the git side of the merge
// on grain's behalf.
func (s *Sim) mergeIntoBase(pr PullRequest) error {
	dir, err := os.MkdirTemp("", "githubsim-merge-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	wd := filepath.Join(dir, "work")
	if err := runGit(dir, "git", "clone", "-q", s.BareRepo, wd); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"git", "config", "user.email", "github@example.com"},
		{"git", "config", "user.name", "github (simulated merge)"},
		{"git", "fetch", "-q", "origin", pr.Head},
		{"git", "checkout", "-q", pr.Base},
		{"git", "merge", "--no-ff", "origin/" + pr.Head, "-m", fmt.Sprintf("Merge pull request #%d from %s", pr.Number, pr.Head)},
		{"git", "push", "-q", "origin", pr.Base},
	} {
		if err := runGit(wd, args[0], args[1:]...); err != nil {
			return err
		}
	}
	return nil
}

// containsBranch reports whether base's own history already contains
// head's tip -- the question real GitHub's merges endpoint answers 204
// for, asked here the honest way (`git merge-base --is-ancestor` against
// the bare repo) rather than from bookkeeping, since "was this branch
// actually behind" is precisely what a caller of that endpoint is
// relying on it for.
func (s *Sim) containsBranch(base, head string) bool {
	return exec.Command("git", "--git-dir", s.BareRepo, "merge-base", "--is-ancestor",
		"refs/heads/"+head, "refs/heads/"+base).Run() == nil
}

// mergeBranches merges head into base for real, against BareRepo, the
// same way mergeIntoBase performs a pull request's own merge -- real
// GitHub's POST /merges writes a merge commit onto base, so a Sim that
// only said it had would let the merge queue believe it refreshed a
// branch that never moved, which is the whole thing the caller then waits
// a cycle for fresh CI on.
//
// The second return is a conflict rather than an error: once both
// branches are known to exist (the caller 404s first) and base is known
// not to contain head already, a conflict is the only way this merge can
// fail, and it is an answer -- real GitHub's own 409 -- not a fault. That
// is the case a test most wants to be real, since it is the one the merge
// queue cannot repair by itself.
func (s *Sim) mergeBranches(base, head, message string) (sha string, conflicted bool, err error) {
	dir, err := os.MkdirTemp("", "githubsim-merges-*")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(dir)
	wd := filepath.Join(dir, "work")
	if err := runGit(dir, "git", "clone", "-q", s.BareRepo, wd); err != nil {
		return "", false, err
	}
	for _, args := range [][]string{
		{"git", "config", "user.email", "github@example.com"},
		{"git", "config", "user.name", "github (simulated merge)"},
		{"git", "fetch", "-q", "origin", head},
		{"git", "checkout", "-q", base},
	} {
		if err := runGit(wd, args[0], args[1:]...); err != nil {
			return "", false, err
		}
	}
	if err := runGit(wd, "git", "merge", "--no-ff", "origin/"+head, "-m", message); err != nil {
		return "", true, nil
	}
	if err := runGit(wd, "git", "push", "-q", "origin", base); err != nil {
		return "", false, err
	}
	return s.branchSHA(base), false, nil
}

// runGit runs a git command in dir, folding its combined output into the
// returned error so a merge conflict or missing branch -- the only ways
// this can fail -- is diagnosable from the *github.Error mergeIntoBase's
// caller turns it into, the same as a real 405 response body would be.
func runGit(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("githubsim: %s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return nil
}

func splitPathQuery(path string) (string, url.Values) {
	p, rawQuery, _ := strings.Cut(path, "?")
	qs, _ := url.ParseQuery(rawQuery)
	return p, qs
}

func issueJSON(number int, owner, repo string, issue *Issue) map[string]any {
	labels := make([]string, 0, len(issue.Labels))
	for l := range issue.Labels {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	labelObjs := make([]map[string]string, len(labels))
	for i, l := range labels {
		labelObjs[i] = map[string]string{"name": l}
	}
	state := issue.State
	if state == "" {
		state = "open"
	}
	return map[string]any{
		"number": number, "title": issue.Title, "body": issue.Body,
		"html_url": fmt.Sprintf("https://github.example/%s/%s/issues/%d", owner, repo, number),
		"labels":   labelObjs, "state": state,
		"user": map[string]string{"login": issue.Author},
	}
}

func jsonResponse(status int, v any) github.ApiResponse {
	body, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return github.ApiResponse{Status: status, Body: body}
}

// Request implements github.Transport. It panics on any request shape it
// doesn't recognise, the same as v1's own RealGitHubMock raising
// AssertionError -- a test exercising an endpoint this double doesn't yet
// answer for should fail loudly, not silently get back a default "[]".
func (s *Sim) Request(method, path string, headers map[string]string, body []byte) (github.ApiResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.Calls = append(s.Calls, Call{Method: method, Path: path})
	p, qs := splitPathQuery(path)

	if method == "GET" {
		if m := issueCommentsRe.FindStringSubmatch(p); m != nil {
			number := mustAtoi(m[3])
			return jsonResponse(200, commentsJSON(s.Comments[number])), nil
		}
		if m := pullsRe.FindStringSubmatch(p); m != nil && qs.Get("head") != "" {
			s.mustOwn(m[1], m[2])
			return jsonResponse(200, s.findOpenPullRequestsForHead(qs.Get("head"))), nil
		}
		if m := pullRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			number := mustAtoi(m[3])
			pr, ok := s.pullRequest(number)
			if !ok {
				return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
			}
			return jsonResponse(200, pullRequestDetailJSON(pr, s.branchSHA(pr.Head))), nil
		}
		if m := pullCommentsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			number := mustAtoi(m[3])
			return jsonResponse(200, reviewCommentsJSON(s.ReviewComments[number])), nil
		}
		if m := checkRunsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			ref, err := url.PathUnescape(m[3])
			if err != nil {
				ref = m[3]
			}
			return jsonResponse(200, checkRunsJSON(s.checkRunsFor(ref))), nil
		}
		if m := actionsRunsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			key := qs.Get("head_sha")
			jobs := s.workflowJobsFor(key)
			return jsonResponse(200, workflowRunsJSON(s.runIDFor(key), jobs)), nil
		}
		if m := runJobsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			runID := mustAtoi(m[3])
			key, ok := s.runKeyFor(runID)
			if !ok {
				return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
			}
			return jsonResponse(200, jobsJSON(m[1], m[2], runID, s.workflowJobsFor(key))), nil
		}
		if m := jobLogsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			id := mustAtoi(m[3])
			runID, index := id/1000, id%1000
			key, ok := s.runKeyFor(runID)
			if !ok {
				return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
			}
			jobs := s.workflowJobsFor(key)
			if index >= len(jobs) || jobs[index].Log == "" {
				// 410, not 404: this is the shape of a real log past its
				// retention window, and the one FailedJobLogs has to skip
				// rather than fail on.
				return github.ApiResponse{Status: 410, Body: []byte("{}")}, nil
			}
			// Plain text, like the real endpoint's own answer after the
			// redirect github's storage serves it from.
			return github.ApiResponse{Status: 200, Body: []byte(jobs[index].Log)}, nil
		}
		if m := issueRe.FindStringSubmatch(p); m != nil {
			number := mustAtoi(m[3])
			issue, ok := s.Issues[number]
			if !ok {
				return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
			}
			return jsonResponse(200, issueJSON(number, s.Owner, s.Repo, issue)), nil
		}
		if m := repoRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			return jsonResponse(200, map[string]string{"default_branch": s.DefaultBranch}), nil
		}
		if m := issuesRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			label := qs.Get("labels")
			numbers := make([]int, 0, len(s.Issues))
			for n := range s.Issues {
				numbers = append(numbers, n)
			}
			sort.Ints(numbers)
			items := make([]map[string]any, 0, len(numbers))
			for _, n := range numbers {
				issue := s.Issues[n]
				if label != "" {
					if _, ok := issue.Labels[label]; !ok {
						continue
					}
				}
				items = append(items, issueJSON(n, s.Owner, s.Repo, issue))
			}
			return jsonResponse(200, items), nil
		}
		if m := branchRe.FindStringSubmatch(p); m != nil {
			branch, err := url.PathUnescape(m[3])
			if err != nil {
				branch = m[3]
			}
			sha, message, ok := s.branchHead(branch)
			if !ok {
				return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
			}
			return jsonResponse(200, map[string]any{
				"commit": map[string]any{
					"sha":    sha,
					"commit": map[string]string{"message": message},
				},
			}), nil
		}
	}

	if method == "POST" {
		if m := issueCommentsRe.FindStringSubmatch(p); m != nil {
			number := mustAtoi(m[3])
			var payload struct {
				Body string `json:"body"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			s.nextCommentID++
			comment := github.Comment{ID: s.nextCommentID, User: "grain-agent", Body: payload.Body, AuthorAssociation: "NONE"}
			s.Comments[number] = append(s.Comments[number], comment)
			return jsonResponse(201, map[string]any{"id": comment.ID, "body": comment.Body}), nil
		}
		if m := issuesRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			var payload struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			s.nextIssue++
			number := s.nextIssue
			issue := &Issue{Title: payload.Title, Body: payload.Body, Labels: labelSetFrom(payload.Labels)}
			s.Issues[number] = issue
			return jsonResponse(201, issueJSON(number, s.Owner, s.Repo, issue)), nil
		}
		if m := labelsPostRe.FindStringSubmatch(p); m != nil {
			number := mustAtoi(m[3])
			var payload struct {
				Labels []string `json:"labels"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			issue := s.Issues[number]
			for _, l := range payload.Labels {
				issue.Labels[l] = struct{}{}
			}
			return github.ApiResponse{Status: 200, Body: []byte("[]")}, nil
		}
		if m := pullsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			var payload struct {
				Title string `json:"title"`
				Head  string `json:"head"`
				Base  string `json:"base"`
				Body  string `json:"body"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			number := 9000 + len(s.PullRequests)
			pr := PullRequest{
				Number: number, Title: payload.Title, Body: payload.Body,
				Head: payload.Head, Base: payload.Base, State: "open",
				HTMLURL:   fmt.Sprintf("https://github.example/%s/%s/pull/%d", s.Owner, s.Repo, number),
				CreatedAt: time.Now(),
			}
			s.PullRequests = append(s.PullRequests, pr)
			return jsonResponse(201, map[string]any{
				"number": pr.Number, "title": pr.Title, "body": pr.Body,
				"head": pr.Head, "base": pr.Base, "html_url": pr.HTMLURL,
			}), nil
		}
		if m := pullReviewsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			number := mustAtoi(m[3])
			var payload struct {
				Body     string `json:"body"`
				Comments []struct {
					Path string `json:"path"`
					Line int    `json:"line"`
					Body string `json:"body"`
				} `json:"comments"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			comments := make([]github.NewReviewComment, len(payload.Comments))
			for i, c := range payload.Comments {
				comments[i] = github.NewReviewComment{Path: c.Path, Line: c.Line, Body: c.Body}
			}
			s.nextReviewID++
			s.Reviews = append(s.Reviews, Review{Number: number, Body: payload.Body, Comments: comments})
			return jsonResponse(200, map[string]any{"id": s.nextReviewID}), nil
		}
		if m := mergesRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			var payload struct {
				Base          string `json:"base"`
				Head          string `json:"head"`
				CommitMessage string `json:"commit_message"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			// A branch that is not in this repository is real GitHub's own
			// 404 here -- what a fork pull request's head reads as, and
			// what the caller has to treat as "cannot refresh this one".
			if !s.branchExists(payload.Base) || !s.branchExists(payload.Head) {
				return github.ApiResponse{Status: 404, Body: []byte(`{"message":"Not Found"}`)}, nil
			}
			if s.containsBranch(payload.Base, payload.Head) {
				return github.ApiResponse{Status: 204, Body: nil}, nil
			}
			message := payload.CommitMessage
			if message == "" {
				message = fmt.Sprintf("Merge %s into %s", payload.Head, payload.Base)
			}
			sha, conflicted, err := s.mergeBranches(payload.Base, payload.Head, message)
			if err != nil {
				return github.ApiResponse{Status: 500, Body: []byte(err.Error())}, nil
			}
			if conflicted {
				return github.ApiResponse{Status: 409, Body: []byte(`{"message":"Merge conflict"}`)}, nil
			}
			return jsonResponse(201, map[string]any{"sha": sha}), nil
		}
		if m := gitRefsRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			var payload struct {
				Ref string `json:"ref"`
				SHA string `json:"sha"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			branch := strings.TrimPrefix(payload.Ref, "refs/heads/")
			if s.branchExists(branch) {
				return github.ApiResponse{Status: 422, Body: []byte(`{"message":"Reference already exists"}`)}, nil
			}
			if err := s.setBranch(branch, payload.SHA); err != nil {
				return github.ApiResponse{Status: 422, Body: []byte(err.Error())}, nil
			}
			return jsonResponse(201, map[string]string{"ref": payload.Ref, "object.sha": payload.SHA}), nil
		}
	}

	if method == "PATCH" {
		if m := issueRe.FindStringSubmatch(p); m != nil {
			number := mustAtoi(m[3])
			var payload struct {
				State string `json:"state"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			issue, ok := s.Issues[number]
			if !ok {
				return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
			}
			issue.State = payload.State
			return jsonResponse(200, issueJSON(number, s.Owner, s.Repo, issue)), nil
		}
		if m := gitRefRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			branch, err := url.PathUnescape(m[3])
			if err != nil {
				branch = m[3]
			}
			var payload struct {
				SHA   string `json:"sha"`
				Force bool   `json:"force"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				return github.ApiResponse{}, err
			}
			if !s.branchExists(branch) {
				return github.ApiResponse{Status: 422, Body: []byte(`{"message":"Reference does not exist"}`)}, nil
			}
			if err := s.setBranch(branch, payload.SHA); err != nil {
				return github.ApiResponse{Status: 422, Body: []byte(err.Error())}, nil
			}
			return jsonResponse(200, map[string]string{"ref": "refs/heads/" + branch, "object.sha": payload.SHA}), nil
		}
	}

	if method == "PUT" {
		if m := pullMergeRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			number := mustAtoi(m[3])
			var payload struct {
				SHA string `json:"sha"`
			}
			if len(body) > 0 {
				if err := json.Unmarshal(body, &payload); err != nil {
					return github.ApiResponse{}, err
				}
			}
			for i := range s.PullRequests {
				if s.PullRequests[i].Number == number {
					// GitHub's own `sha` parameter: "SHA that pull
					// request head must match to allow merge", answered
					// 409 when it does not. A caller that merges on a
					// verdict it computed from one commit's CI sends it
					// (orchestrator.syncEntry), and a branch that moved
					// since then has to be refused here rather than
					// merged, or a test can never tell the two apart.
					if payload.SHA != "" && payload.SHA != s.branchSHA(s.PullRequests[i].Head) {
						return github.ApiResponse{Status: 409, Body: []byte(
							`{"message":"Head branch was modified. Review and try the merge again."}`)}, nil
					}
					if err := s.mergeIntoBase(s.PullRequests[i]); err != nil {
						return github.ApiResponse{Status: 405, Body: []byte(err.Error())}, nil
					}
					s.PullRequests[i].State = "closed"
					s.PullRequests[i].Merged = true
					s.PullRequests[i].MergedAt = time.Now()
					return github.ApiResponse{Status: 200, Body: []byte("{}")}, nil
				}
			}
			return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
		}
	}

	if method == "DELETE" {
		if m := labelDeleteRe.FindStringSubmatch(p); m != nil {
			number := mustAtoi(m[3])
			label, err := url.PathUnescape(m[4])
			if err != nil {
				label = m[4]
			}
			delete(s.Issues[number].Labels, label)
			return github.ApiResponse{Status: 200, Body: []byte("{}")}, nil
		}
	}

	panic(fmt.Sprintf("githubsim: unhandled request %s %s", method, path))
}

// checkRunsFor answers GET .../commits/{ref}/check-runs for a ref that
// may be a branch name or a commit sha, whichever CheckRuns happens to be
// keyed by -- real GitHub resolves both to one commit, and BareRepo is
// where the same resolution lives here.
//
// It matters because the two ends disagree on purpose. A test seeds
// checks against a branch, which is the readable thing to write; the
// orchestrator asks by head sha, because a branch-scoped read answers for
// whatever the branch points at now rather than for the commit whose
// health it is deciding (orchestrator.checkRunsFor). Resolving here keeps
// those tests saying what they mean, while a test that seeds by sha --
// one about a branch that moves mid-cycle -- still gets an answer that is
// strictly about the commit it named, since a moved branch resolves to a
// different sha and finds nothing.
func (s *Sim) checkRunsFor(ref string) []github.CheckRun {
	if runs, ok := s.CheckRuns[ref]; ok {
		return runs
	}
	// ref is a branch whose checks were seeded by sha...
	if sha := s.branchSHA(ref); sha != "" {
		return s.CheckRuns[sha]
	}
	// ...or a sha whose checks were seeded by branch name. Keys in sorted
	// order so two branches sitting on one commit answer the same way
	// every run rather than however the map felt like iterating.
	keys := make([]string, 0, len(s.CheckRuns))
	for key := range s.CheckRuns {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if s.branchSHA(key) == ref {
			return s.CheckRuns[key]
		}
	}
	return nil
}

// pullRequest returns the pull request numbered number, if Sim has one.
func (s *Sim) pullRequest(number int) (PullRequest, bool) {
	for _, pr := range s.PullRequests {
		if pr.Number == number {
			return pr, true
		}
	}
	return PullRequest{}, false
}

// findOpenPullRequestsForHead answers GET .../pulls?head=owner:branch --
// github.RESTClient.FindOpenPullRequestForBranch's own request shape.
// GitHub allows at most one open PR per head branch, but this returns a
// list either way, matching the real endpoint's own shape (a filtered
// list, never a single object).
func (s *Sim) findOpenPullRequestsForHead(head string) []map[string]any {
	_, branch, ok := strings.Cut(head, ":")
	if !ok {
		branch = head
	}
	var out []map[string]any
	for _, pr := range s.PullRequests {
		if pr.Head == branch && pr.State == "open" {
			out = append(out, map[string]any{
				"number": pr.Number, "title": pr.Title, "body": pr.Body,
				"head": pr.Head, "base": pr.Base, "html_url": pr.HTMLURL,
			})
		}
	}
	return out
}

// pullRequestDetailJSON is the shape github.RESTClient.GetPullRequest
// decodes -- head/base as nested {"ref": ...} objects, not the bare
// strings CreatePullRequest's own response uses, matching GitHub's own
// (inconsistent, but real) shape between the two endpoints.
//
// headSHA is the head branch's current tip, read out of the bare repo by
// the caller (there is no other record of it here: this sim stores a pull
// request's head as a branch name, and git is where that name resolves).
// Empty when the branch is gone -- a merged pull request whose head was
// deleted -- which is the same empty GitHub's own response can carry, and
// which orchestrator.checkRunsFor already reads as "no commit to scope
// to."
func pullRequestDetailJSON(pr PullRequest, headSHA string) map[string]any {
	state := pr.State
	if state == "" {
		state = "open"
	}
	out := map[string]any{
		"number": pr.Number, "title": pr.Title, "body": pr.Body, "html_url": pr.HTMLURL,
		"state":     state,
		"head":      map[string]string{"ref": pr.Head, "sha": headSHA},
		"base":      map[string]string{"ref": pr.Base},
		"mergeable": pr.Mergeable,
		"merged":    pr.Merged,
	}
	if !pr.CreatedAt.IsZero() {
		out["created_at"] = pr.CreatedAt.UTC().Format(time.RFC3339)
	}
	if pr.Merged && !pr.MergedAt.IsZero() {
		out["merged_at"] = pr.MergedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// commentsJSON is the shape github.RESTClient.ListComments decodes.
func commentsJSON(comments []github.Comment) []map[string]any {
	out := make([]map[string]any, len(comments))
	for i, c := range comments {
		out[i] = map[string]any{
			"id": c.ID, "body": c.Body, "author_association": c.AuthorAssociation,
			"user": map[string]string{"login": c.User},
		}
	}
	return out
}

// reviewCommentsJSON is the shape github.RESTClient.ListReviewComments
// decodes -- id/user.login/body/path/line, the inline (diff-attached)
// shape distinct from commentsJSON's plain conversation-thread one.
func reviewCommentsJSON(comments []github.ReviewComment) []map[string]any {
	out := make([]map[string]any, len(comments))
	for i, c := range comments {
		out[i] = map[string]any{
			"id": c.ID, "body": c.Body, "path": c.Path, "line": c.Line,
			"user": map[string]string{"login": c.User},
		}
	}
	return out
}

// workflowJobsFor is the jobs seeded for a commit, found by the sha the
// Actions endpoints are called with or by the name of a branch whose tip
// is that sha.
//
// The second half is what makes these seedable at all: a pull request's
// head sha comes out of the bare repo (pullRequestDetailJSON reads it
// with branchSHA), so a test that seeded CheckRuns under a branch name
// has no sha to seed these under. Resolving the key rather than the
// request keeps one seeding convention for both maps.
func (s *Sim) workflowJobsFor(sha string) []WorkflowJob {
	if sha == "" {
		return nil
	}
	if jobs, ok := s.WorkflowJobs[sha]; ok {
		return jobs
	}
	for key, jobs := range s.WorkflowJobs {
		if s.branchSHA(key) == sha {
			return jobs
		}
	}
	return nil
}

// runIDBase keeps Sim's fake workflow-run ids clear of the small integers
// everything else here counts from, so a run id in a failing test's
// output is recognisable as one.
const runIDBase = 9000

// runIDFor is the workflow run id Sim answers for a commit -- stable
// across calls, so the jobs endpoint can be asked about a run the runs
// endpoint named earlier.
func (s *Sim) runIDFor(key string) int {
	for i, k := range s.runKeys {
		if k == key {
			return runIDBase + i
		}
	}
	s.runKeys = append(s.runKeys, key)
	return runIDBase + len(s.runKeys) - 1
}

// runKeyFor inverts runIDFor.
func (s *Sim) runKeyFor(runID int) (string, bool) {
	i := runID - runIDBase
	if i < 0 || i >= len(s.runKeys) {
		return "", false
	}
	return s.runKeys[i], true
}

// jobID numbers a job within its run, so one integer identifies both --
// the logs endpoint is given nothing else to find a job by.
func jobID(runID, index int) int { return runID*1000 + index }

// jobConclusion is GitHub's own field, with Sim's default for a job a
// test seeded without one.
func jobConclusion(job WorkflowJob) string {
	if job.Conclusion == "" {
		return "success"
	}
	return job.Conclusion
}

// workflowRunsJSON is the shape both github.RESTClient.ListWorkflowRuns
// and its FailedJobLogs decode: one run per commit here, since Sim models
// a commit's CI as a flat set of jobs rather than as several workflows.
// Its conclusion follows its jobs -- a run whose jobs all passed is not a
// failed run, and FailedJobLogs never asks it for jobs.
func workflowRunsJSON(runID int, jobs []WorkflowJob) map[string]any {
	if len(jobs) == 0 {
		return map[string]any{"total_count": 0, "workflow_runs": []map[string]any{}}
	}
	conclusion := "success"
	for _, job := range jobs {
		switch jobConclusion(job) {
		case "failure", "timed_out", "startup_failure":
			conclusion = "failure"
		}
	}
	return map[string]any{
		"total_count": 1,
		"workflow_runs": []map[string]any{{
			"id": runID, "name": "tests", "status": "completed", "conclusion": conclusion,
		}},
	}
}

// jobsJSON is the shape github.RESTClient.FailedJobLogs decodes for one
// run's jobs.
func jobsJSON(owner, repo string, runID int, jobs []WorkflowJob) map[string]any {
	items := make([]map[string]any, len(jobs))
	for i, job := range jobs {
		id := jobID(runID, i)
		items[i] = map[string]any{
			"id": id, "name": job.Name, "status": "completed",
			"conclusion": jobConclusion(job),
			"html_url": fmt.Sprintf("https://github.example/%s/%s/actions/runs/%d/job/%d",
				owner, repo, runID, id),
		}
	}
	return map[string]any{"total_count": len(items), "jobs": items}
}

// checkRunsJSON is the shape github.RESTClient.ListCheckRuns decodes --
// GitHub's own "not a bare array" response for this one endpoint.
func checkRunsJSON(runs []github.CheckRun) map[string]any {
	items := make([]map[string]any, len(runs))
	for i, r := range runs {
		items[i] = map[string]any{"name": r.Name, "status": r.Status, "conclusion": r.Conclusion}
	}
	return map[string]any{"total_count": len(items), "check_runs": items}
}

func labelSetFrom(names []string) map[string]struct{} {
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

func (s *Sim) mustOwn(owner, repo string) {
	if owner != s.Owner || repo != s.Repo {
		panic(fmt.Sprintf("githubsim: configured for %s/%s, got %s/%s", s.Owner, s.Repo, owner, repo))
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
