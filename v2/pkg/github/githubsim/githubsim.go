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
// Sim implements exactly the endpoints github.RESTClient calls
// (list/get issue, comments, default branch, add/remove label,
// branch existence, create pull request) -- nothing GitHub's API exposes
// beyond that, since nothing here needs it. Wired in as a
// github.Transport, github.RESTClient's own logic (path building,
// pagination via the shape of a response's own Link header where it
// applies, status-code handling, JSON field extraction) runs completely
// unmodified; only the network call underneath is swapped.
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
	"os/exec"
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
	repoRe        = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)$`)
	issuesRe      = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues$`)
	labelsPostRe  = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)/labels$`)
	labelDeleteRe = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/issues/(\d+)/labels/([^/]+)$`)
	branchRe      = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/branches/(.+)$`)
	pullsRe       = regexp.MustCompile(`^/repos/([^/]+)/([^/]+)/pulls$`)
)

// Issue is one seeded or created fake issue -- Sim's own bookkeeping, not
// github.Issue, since Sim needs a mutable Labels set an incoming request
// can add to or remove from.
type Issue struct {
	Title  string
	Body   string
	Labels map[string]struct{}
}

// PullRequest is one pull request Sim recorded through CreatePullRequest.
type PullRequest struct {
	Number  int
	Title   string
	Body    string
	Head    string
	Base    string
	HTMLURL string
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
}

// New returns a Sim seeded with no issues and no pull requests, answering
// for owner/repo, with defaultBranch as the target repo's own default
// branch and bareRepo as the real git repository BranchExists checks
// against.
func New(owner, repo, bareRepo, defaultBranch string) *Sim {
	return &Sim{
		Owner: owner, Repo: repo, BareRepo: bareRepo, DefaultBranch: defaultBranch,
		Issues: map[int]*Issue{},
	}
}

func (s *Sim) branchExists(branch string) bool {
	cmd := exec.Command("git", "--git-dir", s.BareRepo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
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
	return map[string]any{
		"number": number, "title": issue.Title, "body": issue.Body,
		"html_url": fmt.Sprintf("https://github.example/%s/%s/issues/%d", owner, repo, number),
		"labels":   labelObjs,
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
			// No conversation on any seeded issue; the caller renders the
			// blank state plainly, so an empty list is a real answer.
			return github.ApiResponse{Status: 200, Body: []byte("[]")}, nil
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
				Head: payload.Head, Base: payload.Base,
				HTMLURL: fmt.Sprintf("https://github.example/%s/%s/pull/%d", s.Owner, s.Repo, number),
			}
			s.PullRequests = append(s.PullRequests, pr)
			return jsonResponse(201, map[string]any{
				"number": pr.Number, "title": pr.Title, "body": pr.Body,
				"head": pr.Head, "base": pr.Base, "html_url": pr.HTMLURL,
			}), nil
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
