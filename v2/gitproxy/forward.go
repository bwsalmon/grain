package gitproxy

// Forwards a validated request to the real GitHub smart-HTTP endpoint.
//
// Behind a Forwarder interface for the same reason command execution sits
// behind an interface elsewhere in this project: the proxy's decision
// logic (core.go) is worth testing without a real network call to
// github.com on every test run.

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
)

// UpstreamResponse is what the forward target answered.
type UpstreamResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Forwarder sends one validated request on to the real git host.
type Forwarder interface {
	Forward(method, path, query string, headers map[string]string, body []byte, token *string) (UpstreamResponse, error)
}

// RealForwarder talks to a git smart-HTTP endpoint over HTTPS by default.
// UseTLS false is the git-transport half of the mock-GitHub test seam:
// it lets a live test point a real GitProxy's forward target at a local
// stand-in server instead of github.com.
type RealForwarder struct {
	Host   string
	UseTLS bool
	Client *http.Client
}

// NewRealForwarder returns a forwarder aimed at host over HTTPS.
func NewRealForwarder(host string) *RealForwarder {
	return &RealForwarder{Host: host, UseTLS: true}
}

func (f *RealForwarder) Forward(method, path, query string, headers map[string]string, body []byte, token *string) (UpstreamResponse, error) {
	scheme := "https"
	if !f.UseTLS {
		scheme = "http"
	}
	url := scheme + "://" + f.Host + path
	if query != "" {
		url += "?" + query
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return UpstreamResponse{}, err
	}

	setHeader(req, "Content-Type", headers["Content-Type"])
	accept := headers["Accept"]
	if accept == "" {
		accept = "*/*"
	}
	setHeader(req, "Accept", accept)
	userAgent := headers["User-Agent"]
	if userAgent == "" {
		userAgent = "git/grain-proxy"
	}
	setHeader(req, "User-Agent", userAgent)
	// git gzip-compresses a git-upload-pack request body once its
	// "have"/"want" negotiation gets large enough (http.postBuffer) --
	// forwarding a compressed body without this header means the upstream
	// tries to parse gzip bytes as plain pkt-line data.
	setHeader(req, "Content-Encoding", headers["Content-Encoding"])
	if token != nil {
		// GitHub's HTTPS PAT convention: the token as the Basic auth
		// username, empty password.
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(*token+":")))
	}

	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return UpstreamResponse{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return UpstreamResponse{}, err
	}
	respHeaders := map[string]string{}
	for k := range resp.Header {
		respHeaders[k] = resp.Header.Get(k)
	}
	return UpstreamResponse{Status: resp.StatusCode, Headers: respHeaders, Body: data}, nil
}

func setHeader(req *http.Request, key, value string) {
	if value != "" {
		req.Header.Set(key, value)
	}
}

// FakeForwarder records calls and replays a scripted response. For tests.
type FakeForwarder struct {
	Response UpstreamResponse
	Err      error
	Calls    []ForwardCall
}

// ForwardCall is one recorded call to FakeForwarder.Forward.
type ForwardCall struct {
	Method  string
	Path    string
	Query   string
	Headers map[string]string
	Body    []byte
	Token   *string
}

func (f *FakeForwarder) Forward(method, path, query string, headers map[string]string, body []byte, token *string) (UpstreamResponse, error) {
	f.Calls = append(f.Calls, ForwardCall{Method: method, Path: path, Query: query, Headers: headers, Body: body, Token: token})
	if f.Err != nil {
		return UpstreamResponse{}, f.Err
	}
	return f.Response, nil
}
