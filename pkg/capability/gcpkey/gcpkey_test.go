package gcpkey

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// --- fakes -----------------------------------------------------------

// fakeMinter is an in-memory Minter, standing in for a real GCP project --
// the same "no sandbox and no cloud" bar model/capability_test.go's own
// test capabilities hold to.
type fakeMinter struct {
	nextID     int
	keys       map[string]KeyInfo // id -> info
	createErr  error
	deleteErr  map[string]error // id -> error, for one bad delete
	listErr    error
	minterSeen string // last credential material CreateKey/DeleteKey/ListKeys was authenticated with
}

func newFakeMinter() *fakeMinter {
	return &fakeMinter{keys: map[string]KeyInfo{}}
}

func (m *fakeMinter) CreateKey(ctx context.Context, account string) (string, string, error) {
	if m.createErr != nil {
		return "", "", m.createErr
	}
	m.nextID++
	id := "key-" + string(rune('a'-1+m.nextID))
	m.keys[id] = KeyInfo{ID: id, CreatedAt: time.Now()}
	return id, `{"private_key_id":"` + id + `"}`, nil
}

func (m *fakeMinter) DeleteKey(ctx context.Context, account, keyID string) error {
	if err, ok := m.deleteErr[keyID]; ok {
		return err
	}
	if _, ok := m.keys[keyID]; !ok {
		return errors.New("no such key")
	}
	delete(m.keys, keyID)
	return nil
}

func (m *fakeMinter) ListKeys(ctx context.Context, account string) ([]KeyInfo, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := make([]KeyInfo, 0, len(m.keys))
	for _, k := range m.keys {
		out = append(out, k)
	}
	return out, nil
}

// fakeCredentials resolves exactly the names it is seeded with, recording
// what was asked for -- CredentialResolver's own contract is "what a
// provider can reach is exactly what it names", so a test asserting on
// Resolved is asserting that contract held.
type fakeCredentials struct {
	material map[string]string
	err      error
	resolved []string
}

func (c *fakeCredentials) Resolve(ctx context.Context, name string) (string, error) {
	c.resolved = append(c.resolved, name)
	if c.err != nil {
		return "", c.err
	}
	v, ok := c.material[name]
	if !ok {
		return "", errors.New("no such credential: " + name)
	}
	return v, nil
}

func testProvider(minter *fakeMinter) *Provider {
	p := &Provider{
		Config: Config{
			ServiceAccountEmail: "agent@example-project.iam.gserviceaccount.com",
			ProjectID:           "example-project",
		},
	}
	p.NewMinter = func(ctx context.Context, credentialJSON string) (Minter, error) {
		minter.minterSeen = credentialJSON
		return minter, nil
	}
	return p
}

func testContext(creds model.CredentialResolver, now time.Time) model.CapabilityContext {
	return model.CapabilityContext{
		Task:        model.Task{ID: "t1"},
		Run:         model.Run{ID: "t1-1", TaskID: "t1"},
		Now:         now,
		Credentials: creds,
	}
}

// --- Spec / Resolve ----------------------------------------------------

func TestSpec(t *testing.T) {
	p := NewProvider(Config{MaxKeyAge: 2 * time.Hour})
	spec := p.Spec()
	if spec.Name != "gcp-key" {
		t.Errorf("Name = %q, want gcp-key", spec.Name)
	}
	if spec.Provision != model.ProvisionMint {
		t.Errorf("Provision = %q, want mint", spec.Provision)
	}
	if spec.MaxLease != 2*time.Hour {
		t.Errorf("MaxLease = %v, want the configured 2h", spec.MaxLease)
	}
}

func TestSpecMaxLeaseDefaultsTo24Hours(t *testing.T) {
	p := NewProvider(Config{})
	if got := p.Spec().MaxLease; got != 24*time.Hour {
		t.Errorf("MaxLease = %v, want 24h default", got)
	}
}

func TestResolveRefusesWhenUnconfigured(t *testing.T) {
	p := NewProvider(Config{})
	res, err := p.Resolve(context.Background(), model.CapabilityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused || res.Reason == "" {
		t.Fatalf("got %+v, want a refusal with a human-facing reason", res)
	}
	// A Reason is posted to the task verbatim, so it has to name
	// something that exists: this one named `grain controller configure
	// --gcp-project-id`, which no build of grain has ever had.
	if !strings.Contains(res.Reason, "grain settings -gcp-project") {
		t.Errorf("reason %q does not name the command that actually sets the project", res.Reason)
	}
	if strings.Contains(res.Reason, "labelled") {
		t.Errorf("reason %q still speaks of issue labels, which v2 has none of", res.Reason)
	}
}

func TestResolveHonoursWhenConfigured(t *testing.T) {
	p := testProvider(newFakeMinter())
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "minter-key-material"}}
	res, err := p.Resolve(context.Background(), testContext(creds, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refused {
		t.Fatalf("got %+v, want honoured", res)
	}
}

// A deployment with both settings filled in and the minter secret never
// pasted in is the half-wired case Resolve exists to park rather than
// let Materialize discover: "materializing capabilities: gcpkey:
// resolving minter credential ..." is grain describing its own
// internals, where a task's own comment should name the secret to set.
func TestResolveRefusesWhenTheMinterCredentialIsUnset(t *testing.T) {
	p := testProvider(newFakeMinter())
	creds := &fakeCredentials{material: map[string]string{}}
	res, err := p.Resolve(context.Background(), testContext(creds, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused {
		t.Fatal("expected a refusal when the minter credential resolves to nothing")
	}
	if !strings.Contains(res.Reason, DefaultMinterCredential) {
		t.Errorf("reason %q does not name the credential an operator has to set", res.Reason)
	}
	if !strings.Contains(res.Reason, "grain secrets set") {
		t.Errorf("reason %q does not name the command that sets it", res.Reason)
	}
}

func TestResolveNamesTheConfiguredMinterCredential(t *testing.T) {
	p := testProvider(newFakeMinter())
	p.Config.MinterCredential = "custom-minter"
	res, err := p.Resolve(context.Background(), testContext(&fakeCredentials{}, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused || !strings.Contains(res.Reason, "custom-minter") {
		t.Errorf("got %+v, want a refusal naming this deployment's own minter credential", res)
	}
}

func TestResolveRefusesWithNoResolverAtAll(t *testing.T) {
	p := testProvider(newFakeMinter())
	res, err := p.Resolve(context.Background(), model.CapabilityContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused {
		t.Fatal("expected a refusal rather than a panic when no resolver was wired up")
	}
}

// --- Materialize ---------------------------------------------------------

func TestMaterializeMintsAndPlacesTheKey(t *testing.T) {
	minter := newFakeMinter()
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "minter-key-material"}}
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	m, err := p.Materialize(context.Background(), testContext(creds, now))
	if err != nil {
		t.Fatal(err)
	}

	if len(m.Placements) != 1 {
		t.Fatalf("placements = %+v, want exactly one", m.Placements)
	}
	placement := m.Placements[0]
	if placement.Side != model.SideSandbox {
		t.Errorf("side = %s, want sandbox", placement.Side)
	}
	if placement.Path != SandboxKeyPath {
		t.Errorf("path = %s, want %s", placement.Path, SandboxKeyPath)
	}
	if placement.EffectiveMode() != "600" {
		t.Errorf("mode = %s, want the safe default 600", placement.EffectiveMode())
	}
	if placement.Content == "" {
		t.Error("placement carries no key material")
	}

	if m.Lease == nil {
		t.Fatal("expected a lease")
	}
	if m.Lease.Capability != "gcp-key" {
		t.Errorf("lease capability = %s, want gcp-key", m.Lease.Capability)
	}
	if m.Lease.MintedBy.Name != DefaultMinterCredential {
		t.Errorf("lease minted-by = %s, want %s", m.Lease.MintedBy.Name, DefaultMinterCredential)
	}
	if m.Lease.ExpiresAt == nil || !m.Lease.ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Errorf("lease expiry = %v, want now+24h", m.Lease.ExpiresAt)
	}
	if len(minter.keys) != 1 {
		t.Fatalf("fake project has %d keys, want exactly one minted", len(minter.keys))
	}
	if minter.minterSeen != "minter-key-material" {
		t.Errorf("minter authenticated with %q, want the resolved minter credential", minter.minterSeen)
	}
	if len(creds.resolved) != 1 || creds.resolved[0] != DefaultMinterCredential {
		t.Errorf("resolved credentials = %v, want exactly [%s]", creds.resolved, DefaultMinterCredential)
	}
}

func TestMaterializeUsesConfiguredMinterCredentialName(t *testing.T) {
	minter := newFakeMinter()
	p := testProvider(minter)
	p.Config.MinterCredential = "custom-minter"
	creds := &fakeCredentials{material: map[string]string{"custom-minter": "x"}}

	_, err := p.Materialize(context.Background(), testContext(creds, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if len(creds.resolved) != 1 || creds.resolved[0] != "custom-minter" {
		t.Errorf("resolved = %v, want [custom-minter]", creds.resolved)
	}
}

func TestMaterializeFailsIfCredentialUnresolvable(t *testing.T) {
	p := testProvider(newFakeMinter())
	creds := &fakeCredentials{err: errors.New("no such secret")}
	_, err := p.Materialize(context.Background(), testContext(creds, time.Now()))
	if err == nil {
		t.Fatal("expected an error when the minter credential cannot be resolved")
	}
}

func TestMaterializeFailsIfMintFails(t *testing.T) {
	minter := newFakeMinter()
	minter.createErr = errors.New("quota exceeded")
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}
	_, err := p.Materialize(context.Background(), testContext(creds, time.Now()))
	if err == nil {
		t.Fatal("expected an error when minting fails")
	}
}

// refusedCredentialError is what a stored minter key GCP no longer holds
// the public half of actually produces -- an oauth2 transport error with
// the token endpoint's own body quoted inside it, no googleapi.Error and
// no status code anywhere errors.As can reach. Task 163's failure,
// verbatim but for the project name.
var refusedCredentialError = errors.New(
	`Post "https://iam.googleapis.com/v1/projects/example-project/serviceAccounts/` +
		`agent@example-project.iam.gserviceaccount.com/keys?alt=json&prettyPrint=false": ` +
		`auth: cannot fetch token: 400 Response: ` +
		`{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`)

// The failure this whole explanation exists for. What an operator saw
// was Google's `invalid_grant`, which names no secret, no account and
// nothing about grain -- on a deployment reading **Ready**, because the
// secret is set and only GCP knows the key inside it has stopped
// working.
func TestMaterializeNamesTheSecretWhenGCPRefusesTheMinterCredential(t *testing.T) {
	minter := newFakeMinter()
	minter.createErr = refusedCredentialError
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}

	_, err := p.Materialize(context.Background(), testContext(creds, time.Now()))
	if err == nil {
		t.Fatal("expected a refused credential to fail the mint")
	}
	if !strings.Contains(err.Error(), DefaultMinterCredential) {
		t.Errorf("error %q does not name the secret holding the dead credential", err)
	}
	if !strings.Contains(err.Error(), "grain secrets set") {
		t.Errorf("error %q does not name the command that replaces it", err)
	}
	// Never worse than the original: GCP's own words survive.
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error %q dropped GCP's own message", err)
	}
}

// Revoke authenticates as whatever the lease itself names, so a lease
// minted under a renamed or rotated credential has to report *that*
// name -- pointing an operator at Config's current one would send them
// to replace a secret that is not the one this failed on.
func TestRevokeNamesTheLeasesOwnCredentialWhenGCPRefusesIt(t *testing.T) {
	minter := newFakeMinter()
	minter.keys["key-a"] = KeyInfo{ID: "key-a"}
	minter.deleteErr = map[string]error{"key-a": refusedCredentialError}
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{"older-minter": "x"}}

	err := p.Revoke(context.Background(), testContext(creds, time.Now()), model.Lease{
		Capability: "gcp-key", Resource: "key-a",
		MintedBy: model.CredentialRef{Name: "older-minter"},
	})
	if err == nil {
		t.Fatal("expected a refused credential to fail the revoke")
	}
	if !strings.Contains(err.Error(), "older-minter") {
		t.Errorf("error %q does not name the credential this lease was minted under", err)
	}
}

// Anything else is passed through as it is: wrapping every failure in
// advice about a dead credential would bury the ones that say what they
// are, the same restraint explainCreateFailure shows.
func TestAnOrdinaryFailureIsNotBlamedOnTheCredential(t *testing.T) {
	minter := newFakeMinter()
	minter.createErr = errors.New("googleapi: Error 500: backend error")
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}

	_, err := p.Materialize(context.Background(), testContext(creds, time.Now()))
	if err == nil {
		t.Fatal("expected the mint to fail")
	}
	if strings.Contains(err.Error(), "grain secrets set") {
		t.Errorf("error %q volunteers a credential replacement for a failure that is not one", err)
	}
}

// The mint path used to name the account twice -- iam.go's own
// "minting a key for <account>" wrapped again by Materialize's -- so
// forty duplicated characters of resource name stood between an
// operator and the clause that said what had gone wrong.
func TestMintFailureNamesTheAccountOnlyOnce(t *testing.T) {
	f := newFakeIAM()
	f.createStatus = http.StatusInternalServerError
	f.createBody = `{"error":{"code":500,"message":"backend error","status":"INTERNAL"}}`
	m := testIAMMinter(t, f)
	p := testProvider(newFakeMinter())
	p.NewMinter = func(ctx context.Context, credentialJSON string) (Minter, error) { return m, nil }
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}

	_, err := p.Materialize(context.Background(), testContext(creds, time.Now()))
	if err == nil {
		t.Fatal("expected the mint to fail")
	}
	if n := strings.Count(err.Error(), "gcpkey: minting a key for"); n != 1 {
		t.Errorf("error %q says %q %d times, want once", err, "gcpkey: minting a key for", n)
	}
}

// --- PromptSection ---------------------------------------------------

func TestPromptSectionNamesThePathAndNoSecret(t *testing.T) {
	p := testProvider(newFakeMinter())
	text, err := p.PromptSection(context.Background(), model.CapabilityContext{}, []model.Placement{
		{Side: model.SideSandbox, Path: SandboxKeyPath, Content: "super-secret-private-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(text, SandboxKeyPath) {
		t.Errorf("prompt section = %q, want it to name %s", text, SandboxKeyPath)
	}
	if contains(text, "super-secret-private-key") {
		t.Fatalf("prompt section leaked key material: %q", text)
	}
}

// A key with no project selected is a key every gcloud command fails on,
// with a message that names no credential -- see PromptSection's own doc
// comment.
func TestPromptSectionNamesTheProject(t *testing.T) {
	p := testProvider(newFakeMinter())
	text, err := p.PromptSection(context.Background(), model.CapabilityContext{}, []model.Placement{
		{Side: model.SideSandbox, Path: SandboxKeyPath, Content: "super-secret-private-key"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(text, "gcloud config set project example-project") {
		t.Errorf("prompt section = %q, want it to set the project", text)
	}
	if !contains(text, "GOOGLE_CLOUD_PROJECT=example-project") {
		t.Errorf("prompt section = %q, want it to name the project for SDKs too", text)
	}
}

func TestPromptSectionRejectsWrongPlacementCount(t *testing.T) {
	p := testProvider(newFakeMinter())
	if _, err := p.PromptSection(context.Background(), model.CapabilityContext{}, nil); err == nil {
		t.Fatal("expected an error for zero placements")
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Revoke ------------------------------------------------------------

func TestRevokeDeletesTheLeasedKey(t *testing.T) {
	minter := newFakeMinter()
	minter.keys["key-a"] = KeyInfo{ID: "key-a", CreatedAt: time.Now()}
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{"minted-by-cred": "material"}}

	err := p.Revoke(context.Background(), testContext(creds, time.Now()), model.Lease{
		Resource: "key-a",
		MintedBy: model.CredentialRef{Name: "minted-by-cred"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, stillThere := minter.keys["key-a"]; stillThere {
		t.Error("key-a should have been deleted")
	}
	if len(creds.resolved) != 1 || creds.resolved[0] != "minted-by-cred" {
		t.Errorf("resolved = %v, want [minted-by-cred] -- Revoke should use the lease's own MintedBy", creds.resolved)
	}
}

func TestRevokePropagatesDeleteFailure(t *testing.T) {
	minter := newFakeMinter()
	minter.deleteErr = map[string]error{"key-a": errors.New("already gone")}
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{"c": "material"}}

	err := p.Revoke(context.Background(), testContext(creds, time.Now()), model.Lease{
		Resource: "key-a", MintedBy: model.CredentialRef{Name: "c"},
	})
	if err == nil {
		t.Fatal("expected the delete failure to propagate")
	}
}

// --- Reap ----------------------------------------------------------------

func TestReapDeletesOnlyKeysOlderThanMaxAge(t *testing.T) {
	minter := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	minter.keys["old"] = KeyInfo{ID: "old", CreatedAt: now.Add(-25 * time.Hour)}
	minter.keys["boundary"] = KeyInfo{ID: "boundary", CreatedAt: now.Add(-24 * time.Hour)}
	minter.keys["fresh"] = KeyInfo{ID: "fresh", CreatedAt: now.Add(-1 * time.Hour)}
	minter.keys["unknown"] = KeyInfo{ID: "unknown"} // zero CreatedAt: left alone

	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}

	deleted, err := p.Reap(context.Background(), creds, now)
	if err != nil {
		t.Fatal(err)
	}
	wantDeleted := map[string]bool{"old": true, "boundary": true}
	if len(deleted) != len(wantDeleted) {
		t.Fatalf("deleted = %v, want exactly %v", deleted, wantDeleted)
	}
	for _, id := range deleted {
		if !wantDeleted[id] {
			t.Errorf("unexpectedly deleted %s", id)
		}
	}
	for id := range wantDeleted {
		if _, stillThere := minter.keys[id]; stillThere {
			t.Errorf("%s should have been reaped", id)
		}
	}
	if _, ok := minter.keys["fresh"]; !ok {
		t.Error("fresh should not have been reaped")
	}
	if _, ok := minter.keys["unknown"]; !ok {
		t.Error("a key with no known creation time should be left alone, not reaped")
	}
}

func TestReapIsBestEffortPerKey(t *testing.T) {
	minter := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	minter.keys["bad"] = KeyInfo{ID: "bad", CreatedAt: now.Add(-48 * time.Hour)}
	minter.keys["good"] = KeyInfo{ID: "good", CreatedAt: now.Add(-48 * time.Hour)}
	minter.deleteErr = map[string]error{"bad": errors.New("transient GCP error")}

	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}

	deleted, err := p.Reap(context.Background(), creds, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "good" {
		t.Fatalf("deleted = %v, want exactly [good] -- one failure must not stop the rest", deleted)
	}
	if _, stillThere := minter.keys["bad"]; !stillThere {
		t.Error("bad should still be present -- its delete failed")
	}
}

func TestReapUsesConfiguredMaxAge(t *testing.T) {
	minter := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	minter.keys["k"] = KeyInfo{ID: "k", CreatedAt: now.Add(-2 * time.Hour)}
	p := testProvider(minter)
	p.Config.MaxKeyAge = 1 * time.Hour
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}

	deleted, err := p.Reap(context.Background(), creds, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "k" {
		t.Fatalf("deleted = %v, want [k] under the configured 1h max age", deleted)
	}
}

func TestReapFailsIfListingFails(t *testing.T) {
	minter := newFakeMinter()
	minter.listErr = errors.New("network error")
	p := testProvider(minter)
	creds := &fakeCredentials{material: map[string]string{DefaultMinterCredential: "x"}}
	if _, err := p.Reap(context.Background(), creds, time.Now()); err == nil {
		t.Fatal("expected an error when listing keys fails")
	}
}

// --- Reaper interface ------------------------------------------------

func TestProviderSatisfiesReaper(t *testing.T) {
	var _ model.Reaper = (*Provider)(nil)
}

func TestProviderSatisfiesCapabilityProvider(t *testing.T) {
	var _ model.CapabilityProvider = (*Provider)(nil)
}
