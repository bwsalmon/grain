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
// create/find/get/merge a pull request, the plain comment thread, inline
// review comments and draft reviews, and check runs -- the whole surface
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

	"github.com/bwsalmon/grain/v2/pkg/github"
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
	// GitHub's own merge button the way v2/e2e's mergeBranchIntoDefault
	// does for the git side of a merge.
	State string
	// Mergeable stands in for GitHub's own asynchronously computed field
	// -- nil (unknown) unless a test sets it, since Sim has no real merge
	// engine to compute it with.
	Mergeable *bool
}

// Call is one request Sim answered, kept for tests that want to assert on
// what was actually asked of it.
type Call struct {
	Method string
	Path   string
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
	// name or a sha, matching whatever ListCheckRuns is called with, since
	// Sim has no real commit graph to resolve one into the other.
	CheckRuns map[string][]github.CheckRun
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

// mergeIntoBase performs pr's merge for real, at the git level, against
// BareRepo -- real GitHub's own PUT .../merge endpoint moves commits into
// the base branch as a side effect, not just github.PullRequestDetail's
// State; a Sim that only flipped State to "closed" would let a caller
// believe a merge landed when the base branch never actually moved, which
// is exactly the gap an end-to-end test asserting on the base branch's
// own git log (v2/e2e's own harness already does this for a human's
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
			return jsonResponse(200, pullRequestDetailJSON(pr)), nil
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
			return jsonResponse(200, checkRunsJSON(s.CheckRuns[ref])), nil
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
			if s.branchExists(branch) {
				return github.ApiResponse{Status: 200, Body: []byte("{}")}, nil
			}
			return github.ApiResponse{Status: 404, Body: []byte("{}")}, nil
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
				HTMLURL: fmt.Sprintf("https://github.example/%s/%s/pull/%d", s.Owner, s.Repo, number),
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
	}

	if method == "PUT" {
		if m := pullMergeRe.FindStringSubmatch(p); m != nil {
			s.mustOwn(m[1], m[2])
			number := mustAtoi(m[3])
			for i := range s.PullRequests {
				if s.PullRequests[i].Number == number {
					if err := s.mergeIntoBase(s.PullRequests[i]); err != nil {
						return github.ApiResponse{Status: 405, Body: []byte(err.Error())}, nil
					}
					s.PullRequests[i].State = "closed"
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
func pullRequestDetailJSON(pr PullRequest) map[string]any {
	state := pr.State
	if state == "" {
		state = "open"
	}
	return map[string]any{
		"number": pr.Number, "title": pr.Title, "body": pr.Body, "html_url": pr.HTMLURL,
		"state":     state,
		"head":      map[string]string{"ref": pr.Head},
		"base":      map[string]string{"ref": pr.Base},
		"mergeable": pr.Mergeable,
	}
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
