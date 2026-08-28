package gitproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func strPtr(s string) *string { return &s }

func TestRealForwarderSendsMethodPathQueryAndBody(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("X-Test", "yes")
		w.WriteHeader(201)
		w.Write([]byte("upstream body"))
	}))
	defer server.Close()

	f := &RealForwarder{Host: server.Listener.Addr().String(), UseTLS: false}
	resp, err := f.Forward("POST", "/acme/widgets.git/git-upload-pack", "service=git-upload-pack",
		map[string]string{"Content-Type": "application/x-git-upload-pack-request"},
		[]byte("the body"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotPath != "/acme/widgets.git/git-upload-pack" || gotQuery != "service=git-upload-pack" {
		t.Errorf("got method=%q path=%q query=%q", gotMethod, gotPath, gotQuery)
	}
	if string(gotBody) != "the body" {
		t.Errorf("got body = %q", gotBody)
	}
	if resp.Status != 201 || string(resp.Body) != "upstream body" || resp.Headers["X-Test"] != "yes" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestRealForwarderSetsBasicAuthFromToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
	}))
	defer server.Close()

	f := &RealForwarder{Host: server.Listener.Addr().String(), UseTLS: false}
	if _, err := f.Forward("GET", "/x", "", nil, nil, strPtr("secret-token")); err != nil {
		t.Fatal(err)
	}
	username, password, ok := parseBasicAuthHeader(gotAuth)
	if !ok || username != "secret-token" || password != "" {
		t.Errorf("got auth = %q (user=%q pass=%q ok=%v)", gotAuth, username, password, ok)
	}
}

func TestRealForwarderSendsNoAuthorizationWithoutAToken(t *testing.T) {
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization") != ""
	}))
	defer server.Close()

	f := &RealForwarder{Host: server.Listener.Addr().String(), UseTLS: false}
	if _, err := f.Forward("GET", "/x", "", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if sawAuth {
		t.Error("expected no Authorization header without a token")
	}
}

func TestFakeForwarderRecordsCallsAndReplaysAScriptedResponse(t *testing.T) {
	fake := &FakeForwarder{Response: UpstreamResponse{Status: 200, Body: []byte("hi")}}
	resp, err := fake.Forward("GET", "/a/b.git/info/refs", "service=git-upload-pack",
		map[string]string{"User-Agent": "git/2.40"}, nil, strPtr("tok"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || string(resp.Body) != "hi" {
		t.Errorf("resp = %+v", resp)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].Path != "/a/b.git/info/refs" || *fake.Calls[0].Token != "tok" {
		t.Errorf("Calls = %+v", fake.Calls)
	}
}

func parseBasicAuthHeader(header string) (username, password string, ok bool) {
	req := &http.Request{Header: http.Header{"Authorization": []string{header}}}
	return req.BasicAuth()
}
