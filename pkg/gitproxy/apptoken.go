package gitproxy

// GitHub App authentication: minting a short-lived installation access
// token from an App's own private key, so a credential in
// secrets/github/ can be an App installation instead of a bare PAT.
//
// terraform/gcp/README.md's "What the PAT needs" section explains why
// this exists: a fine-grained PAT has no "Checks" permission to grant at
// all (GitHub offered one initially and withdrew it -- only GitHub Apps
// may use that API in the meantime), so ListCheckRuns returns a permanent
// 403 to a PAT-backed deployment -- the fact bwsalmon/agents#483 surfaced
// as a UI warning (AutoMergeDegraded) rather than fixed, because fixing
// it needed a credential kind this package didn't have yet. This is that
// credential kind (bwsalmon/agents#491).
//
// An App is no longer the *only* way to get auto-merge working, though:
// pkg/orchestrator's checkRunsFor now falls back to the Actions API,
// which a fine-grained PAT can reach with "Actions" read, so a deployment
// whose CI is GitHub Actions needs no App. What still needs one is CI
// reported through the Checks API by anything other than Actions -- a
// third-party provider, or a review bot -- which that fallback cannot
// see and only a checks-capable credential can.
//
// GitHub's own flow ("Authenticating as a GitHub App" in its REST API
// docs): sign a short-lived RS256 JWT claiming iss=<app ID>, then
// exchange it for an installation access token via
// POST /app/installations/{installation_id}/access_tokens. The token
// that comes back is bearer-shaped exactly like a PAT -- RealForwarder's
// Basic-auth convention and RESTClient's Authorization header both
// already send whatever string Credential.Token holds, with no
// PAT-versus-App-token distinction anywhere in either -- but it expires
// in about an hour, so unlike a *.token file's contents it cannot just be
// read once and cached forever; see CredentialSet.load for the refresh.

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/bwsalmon/grain/pkg/github"
)

// AppCredential is one GitHub App installation this deployment can mint
// installation tokens for. AppID and InstallationID are kept as strings,
// as GitHub itself prints them and as the JSON below carries them,
// because neither is ever arithmetic.
type AppCredential struct {
	AppID          string
	InstallationID string
	PrivateKey     *rsa.PrivateKey
}

// ParseAppPrivateKey parses the PEM-encoded RSA private key GitHub
// generates when an App's private key is created. GitHub's own download
// is PKCS#1 ("RSA PRIVATE KEY"); PKCS#8 ("PRIVATE KEY") is accepted too,
// since a key manager an operator routes the download through may
// re-encode it.
func ParseAppPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("gitproxy: no PEM block found in GitHub App private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("gitproxy: parsing GitHub App private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("gitproxy: GitHub App private key is not an RSA key")
	}
	return rsaKey, nil
}

func base64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

// appJWT signs the short-lived JSON Web Token GitHub's App-authentication
// flow exchanges for an installation access token.
//
// iat is backdated a minute -- GitHub's own docs call this out -- so
// clock drift that puts this host slightly ahead of GitHub's own clock
// cannot land iat in GitHub's future and get the JWT rejected as
// "not yet valid". exp is nine minutes out, under GitHub's ten-minute
// ceiling with a minute of margin: nothing here holds onto the JWT past
// the one mintInstallationToken call that immediately follows, so the
// gap between "signed" and "GitHub receives it" is the only clock skew
// that matters, not this token's own lifetime.
func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64URL([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", fmt.Errorf("gitproxy: encoding GitHub App JWT claims: %w", err)
	}
	signingInput := header + "." + base64URL(claims)
	hashed := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hashed[:])
	if err != nil {
		return "", fmt.Errorf("gitproxy: signing GitHub App JWT: %w", err)
	}
	return signingInput + "." + base64URL(sig), nil
}

// mintInstallationToken exchanges cred's App JWT for a fresh installation
// access token over transport, which always talks to GitHub's own REST
// API host: App authentication has no GitHub Enterprise path in this
// package the way ordinary REST and git calls at least attempt via
// github.APIHost, so a transport aimed anywhere else (CredentialSet's own
// tests pass a github.FakeTransport) is a deliberate test seam, not a
// supported deployment shape.
func mintInstallationToken(transport github.Transport, cred AppCredential, now time.Time) (token string, expiresAt time.Time, err error) {
	jwt, err := appJWT(cred.AppID, cred.PrivateKey, now)
	if err != nil {
		return "", time.Time{}, err
	}
	resp, err := transport.Request(
		"POST", fmt.Sprintf("/app/installations/%s/access_tokens", cred.InstallationID),
		map[string]string{
			"Authorization": "Bearer " + jwt,
			"Accept":        "application/vnd.github+json",
		}, nil,
	)
	if err != nil {
		return "", time.Time{}, err
	}
	if resp.Status != 201 {
		return "", time.Time{}, &github.Error{Status: resp.Status, Body: resp.Body}
	}
	var data struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(resp.Body, &data); err != nil {
		return "", time.Time{}, fmt.Errorf("gitproxy: decoding installation token response: %w", err)
	}
	expiresAt, err = time.Parse(time.RFC3339, data.ExpiresAt)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gitproxy: parsing installation token expiry %q: %w", data.ExpiresAt, err)
	}
	return data.Token, expiresAt, nil
}
