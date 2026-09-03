// controller.go is "grain controller ...": one-shot setup verbs an
// operator runs by hand against their own machine, never against a
// running daemon -- unlike every task-management verb in main.go, these
// need no store connection at all. bootstrap-github-app is the first
// and, for now, only one: see pkg/capability/githubsandbox's own doc
// comment for why a GitHub App -- not the "username and password"
// bwsalmon/agents#354 first asked for -- is what that capability's two
// secrets hold, and why this command exists to make registering one a
// single click rather than the account's actual password ever reaching
// grain.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/browser"

	"github.com/bwsalmon/grain/pkg/capability/githubsandbox"
	"github.com/bwsalmon/grain/pkg/secrets"
)

// manifestPermissions is exactly what pkg/capability/githubsandbox's own
// app.go ever asks an installation token for, across
// createRepoPermissions, agentPermissions, and deleteRepoPermissions
// (that package's own doc comment) -- the App this command registers
// needs no permission beyond the union of those three, and granting it
// only that union is what keeps "should not be able to escalate their
// permissions" true one level up, at the App's own ceiling: nothing
// minted from this App, ever, can exceed what is granted here.
var manifestPermissions = map[string]string{
	"contents":                    "write",
	"issues":                      "write",
	"pull_requests":               "write",
	"secrets":                     "write",
	"administration":              "write", // repo-level: only Revoke/Reap's controller-side delete token ever carries this
	"organization_administration": "write", // org-level: only the controller-side create/list token ever carries this
}

// callbackTimeout bounds how long bootstrapGitHubApp waits for the
// manifest flow's browser redirect to reach the local callback server
// before giving up -- long enough for a human to notice the opened tab,
// log in if needed, and click through GitHub's own "Create GitHub App"
// confirmation, short enough that an operator who closed the tab isn't
// left wondering why the command is still running an hour later.
const callbackTimeout = 5 * time.Minute

func controller(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: grain controller bootstrap-github-app [flags]")
		os.Exit(1)
	}
	switch args[0] {
	case "bootstrap-github-app":
		if err := bootstrapGitHubApp(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "grain controller bootstrap-github-app: "+err.Error())
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "grain controller: unknown command %q\n", args[0])
		os.Exit(1)
	}
}

// bootstrapGitHubApp drives GitHub's own App-manifest flow
// (https://docs.github.com/en/apps/sharing-github-apps/registering-a-github-app-from-a-manifest):
// it opens a browser tab that POSTs a manifest -- name and
// manifestPermissions, nothing else -- to github.com, waits for the
// redirect that flow ends with (caught by a local, loopback-only HTTP
// server, never grain's own daemon), and exchanges the resulting
// one-time code for the App's id and private key over the real GitHub
// API. Nothing here ever sees, asks for, or transmits the bot account's
// password -- the operator's only action is logging into that account
// in their own browser (if not already) and clicking the one button
// GitHub itself renders.
//
// The App this produces is not yet installed on any account -- creating
// an App and installing it are two separate GitHub actions, and the
// manifest flow only ever does the first. bootstrapGitHubApp prints the
// App's own settings URL, where "Install App" is one more click, as its
// last step.
func bootstrapGitHubApp(args []string) error {
	fs := flag.NewFlagSet("grain controller bootstrap-github-app", flag.ExitOnError)
	name := fs.String("name", "grain-sandbox", "name for the new GitHub App")
	homepage := fs.String("url", "https://github.com/bwsalmon/grain", "homepage URL the App manifest requires")
	dataDir := fs.String("data-dir", "", "root directory a colocated `grain daemon` was started with -- the App's credentials are written into <data-dir>/secrets, the same store `grain daemon` and `grain secrets` read and write (required)")
	githubHost := fs.String("github-host", "github.com", "GitHub host to run the manifest flow against")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		return fmt.Errorf("-data-dir is required")
	}

	state, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("generating a state token: %w", err)
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			errCh <- fmt.Errorf("callback state %q did not match the request grain made", got)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			errCh <- fmt.Errorf("callback carried no code")
			return
		}
		fmt.Fprintln(w, "GitHub App created. You can close this tab and return to grain.")
		codeCh <- code
	})
	listener, err := newLoopbackListener()
	if err != nil {
		return fmt.Errorf("starting a local callback server: %w", err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(listener)
	defer server.Close()
	redirectURL := fmt.Sprintf("http://%s/callback", listener.Addr().String())

	manifest := map[string]any{
		"name":                *name,
		"url":                 *homepage,
		"public":              false,
		"default_events":      []string{},
		"default_permissions": manifestPermissions,
		"redirect_url":        redirectURL,
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("building the App manifest: %w", err)
	}

	submitURL := fmt.Sprintf("https://%s/settings/apps/new?state=%s", *githubHost, state)
	form := manifestSubmissionForm(submitURL, string(manifestJSON))
	fmt.Printf(
		"Opening a browser to register a new GitHub App named %q. Log in as the bot\n"+
			"account this capability should act as, if you are not already, and click\n"+
			"\"Create GitHub App\" -- grain never sees that account's password.\n\n"+
			"If a browser did not open, POST the manifest below to %s yourself.\n\n",
		*name, submitURL,
	)
	if err := browser.OpenReader(strings.NewReader(form)); err != nil {
		fmt.Printf("(could not open a browser automatically: %v)\n\nmanifest:\n%s\n", err, manifestJSON)
	}

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		return err
	case <-time.After(callbackTimeout):
		return fmt.Errorf("timed out after %s waiting for GitHub's redirect -- rerun this command", callbackTimeout)
	}

	appID, privateKey, htmlURL, err := convertManifest(*githubHost, code)
	if err != nil {
		return fmt.Errorf("exchanging the manifest code: %w", err)
	}

	if err := writeAppCredentials(*dataDir, appID, privateKey); err != nil {
		return err
	}

	fmt.Printf(
		"\nWrote the %s and %s secrets to %s --\n"+
			"the github-sandbox capability's own credentials, ready for a `grain daemon`\n"+
			"(or `grain secrets list`) started against the same -data-dir to pick up.\n\n"+
			"One step left: the App exists but is not installed anywhere yet. Visit\n"+
			"%s, choose \"Install App\", and install it on the same bot\n"+
			"account -- \"All repositories\" (there is nothing to select yet, since this\n"+
			"capability creates repos on demand). Nothing dispatches against\n"+
			"github-sandbox until that install completes.\n",
		githubsandbox.DefaultAppIDCredential, githubsandbox.DefaultPrivateKeyCredential, filepath.Join(*dataDir, "secrets"), htmlURL,
	)
	return nil
}

// writeAppCredentials stores appID and privateKey in the same
// pkg/secrets file (secretsConfig's own two paths) that
// githubsandbox.Provider.Resolve -- and `grain secrets` -- read from,
// under the one "github-app" secret's "app-id" and "private-key" keys:
// exactly the pair DefaultAppIDCredential and DefaultPrivateKeyCredential
// name. Earlier, this command wrote plain files under <secrets-dir>/
// github-app/ instead, which nothing that resolves credentials through
// pkg/secrets.Store (one encrypted file, not a directory of files) ever
// read -- an operator had to separately run `grain secrets set` by hand
// for bootstrap-github-app's output to take effect at all.
func writeAppCredentials(dataDir, appID, privateKey string) error {
	store := secrets.Open(secretsConfig(dataDir))
	if err := store.Set("github-app", "app-id", []byte(appID)); err != nil {
		return fmt.Errorf("writing app-id: %w", err)
	}
	if err := store.Set("github-app", "private-key", []byte(privateKey)); err != nil {
		return fmt.Errorf("writing private-key: %w", err)
	}
	return nil
}

// convertManifest exchanges a one-time manifest code for the new App's
// credentials -- the one real API call in the whole flow
// (docs.github.com's own "app-manifests/{code}/conversions"),
// unauthenticated beyond the code itself, which only GitHub's redirect
// (never anything grain constructs) can produce.
func convertManifest(host, code string) (appID, privateKey, htmlURL string, err error) {
	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("https://%s/app-manifests/%s/conversions", apiHost(host), code), nil)
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", "", err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", "", "", fmt.Errorf("unexpected status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		ID      int64  `json:"id"`
		PEM     string `json:"pem"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", "", fmt.Errorf("parsing the conversion response: %w", err)
	}
	if out.ID == 0 || out.PEM == "" {
		return "", "", "", fmt.Errorf("conversion response carried no id/pem: %s", body)
	}
	return fmt.Sprintf("%d", out.ID), out.PEM, out.HTMLURL, nil
}

// apiHost turns "github.com" into "api.github.com" -- the one host
// whose REST API lives on a different hostname than its web UI -- and
// leaves anything else (a local mock server standing in for GitHub,
// -github-host's own "override to point at a mock for local testing")
// unchanged, matching how pkg/github.RealTransport already treats that
// flag: one hostname serving every path uniformly.
func apiHost(host string) string {
	if host == "github.com" {
		return "api.github.com"
	}
	return host
}

// manifestSubmissionForm is a minimal HTML page that POSTs manifestJSON
// to submitURL the instant it loads -- the only way GitHub's manifest
// flow accepts a manifest (docs.github.com: "a form... with a manifest
// parameter"), so a real form submission, not a GET, is unavoidable.
func manifestSubmissionForm(submitURL, manifestJSON string) string {
	escaped := strings.NewReplacer(`&`, "&amp;", `<`, "&lt;", `>`, "&gt;", `"`, "&quot;").Replace(manifestJSON)
	return fmt.Sprintf(`<!DOCTYPE html>
<html><body onload="document.forms[0].submit()">
Redirecting to GitHub to create the App&hellip;
<form action=%q method="post">
<input type="hidden" name="manifest" value="%s">
</form>
</body></html>`, submitURL, escaped)
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// newLoopbackListener binds 127.0.0.1:0 -- loopback only, the same
// "never reachable from outside this machine" bound
// cmd/grain/daemon.go's own startGitProxy holds its callback server to,
// since this one briefly carries a live manifest-conversion code.
func newLoopbackListener() (net.Listener, error) {
	return net.Listen("tcp", "127.0.0.1:0")
}
