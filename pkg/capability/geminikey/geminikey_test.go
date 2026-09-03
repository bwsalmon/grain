package geminikey

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bwsalmon/grain/pkg/model"
)

// --- fakes -------------------------------------------------------------

// fakeResolver stands in for a real model.CredentialResolver: material
// keyed by name, never touching a filesystem or a network.
type fakeResolver map[string]string

func (f fakeResolver) Resolve(ctx context.Context, name string) (string, error) {
	v, ok := f[name]
	if !ok {
		return "", errors.New("no credential named " + name)
	}
	return v, nil
}

// fakeMinter replaces the real API Keys client so Materialize, Revoke
// and DeleteExpired are testable with no sandbox and no cloud -- the bar
// docs/data-model.md sets for a MINT provider's own tests.
type fakeMinter struct {
	keys      map[string]mintedKey
	strings   map[string]string
	nextID    int
	createErr error
	stringErr error
	deleteErr error
	listErr   error
	deleted   []string
}

func newFakeMinter() *fakeMinter {
	return &fakeMinter{keys: map[string]mintedKey{}, strings: map[string]string{}}
}

func (f *fakeMinter) CreateKey(ctx context.Context, displayName, apiTargetService string) (string, string, error) {
	if f.createErr != nil {
		return "", "", f.createErr
	}
	f.nextID++
	name := "projects/p/locations/global/keys/fake-" + time.Now().Format("150405") + "-" + itoa(f.nextID)
	if f.stringErr != nil {
		return "", "", f.stringErr
	}
	f.keys[name] = mintedKey{Name: name, DisplayName: displayName, CreateTime: time.Now()}
	f.strings[name] = "fake-key-string-" + itoa(f.nextID)
	return name, f.strings[name], nil
}

func (f *fakeMinter) DeleteKey(ctx context.Context, name string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.keys, name)
	delete(f.strings, name)
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeMinter) ListKeys(ctx context.Context) ([]mintedKey, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]mintedKey, 0, len(f.keys))
	for _, k := range f.keys {
		out = append(out, k)
	}
	return out, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- fixtures ------------------------------------------------------------

func testCapability(m minter) *Capability {
	return &Capability{
		ProjectID:  "test-project",
		Credential: model.CredentialRef{Name: "test-gcp-credential"},
		factory: func(ctx context.Context, credentialJSON, projectID string) (minter, error) {
			if credentialJSON != "the-minter-key-material" {
				return nil, errors.New("unexpected credential material: " + credentialJSON)
			}
			if projectID != "test-project" {
				return nil, errors.New("unexpected project id: " + projectID)
			}
			return m, nil
		},
	}
}

func testContext(runID string) model.CapabilityContext {
	return model.CapabilityContext{
		Task: model.Task{ID: "t1"},
		Run:  model.Run{ID: runID, TaskID: "t1"},
		Now:  time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC),
		Credentials: fakeResolver{
			"test-gcp-credential": "the-minter-key-material",
		},
	}
}

// --- Spec ----------------------------------------------------------------

func TestSpec(t *testing.T) {
	c := New("p", model.CredentialRef{Name: "cred"})
	spec := c.Spec()
	if spec.Name != "gemini-key" {
		t.Errorf("Name = %q, want gemini-key", spec.Name)
	}
	if spec.Label != "grain-gemini-key" {
		t.Errorf("Label = %q, want grain-gemini-key", spec.Label)
	}
	if spec.Provision != model.ProvisionMint {
		t.Errorf("Provision = %q, want mint", spec.Provision)
	}
	if spec.Source != model.GrantByLabel {
		t.Errorf("Source = %q, want label", spec.Source)
	}
	if spec.MaxLease != 24*time.Hour {
		t.Errorf("MaxLease = %v, want 24h", spec.MaxLease)
	}
	if want := []string{"cred"}; !reflect.DeepEqual(spec.Requires, want) {
		t.Errorf("Requires = %v, want %v", spec.Requires, want)
	}
}

func TestSpecRequiresOmitsAnUnconfiguredCredential(t *testing.T) {
	c := New("p", model.CredentialRef{})
	if got := c.Spec().Requires; got != nil {
		t.Errorf("Requires = %v, want nil for a Capability with no standing credential configured", got)
	}
}

// --- Resolve ---------------------------------------------------------------

func TestResolveHonoursWhenConfigured(t *testing.T) {
	c := New("test-project", model.CredentialRef{Name: "test-gcp-credential"})
	res, err := c.Resolve(context.Background(), testContext("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Refused {
		t.Fatalf("expected Resolve to honour a configured Capability, got refused: %s", res.Reason)
	}
}

func TestResolveRefusesWithoutProjectID(t *testing.T) {
	c := New("", model.CredentialRef{Name: "test-gcp-credential"})
	res, err := c.Resolve(context.Background(), testContext("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused || res.Reason == "" {
		t.Fatalf("expected a refusal with a human-facing reason, got %+v", res)
	}
}

func TestResolveRefusesWithoutCredential(t *testing.T) {
	c := New("test-project", model.CredentialRef{})
	res, err := c.Resolve(context.Background(), testContext("r1"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused {
		t.Fatal("expected a refusal when no standing credential is configured")
	}
}

// A deployment with a project set and the minter secret never pasted in
// is the half-wired case Resolve exists to park rather than let
// Materialize discover: the reason is posted to the task verbatim, so it
// has to name the secret an operator is meant to set.
func TestResolveRefusesWhenTheCredentialIsUnset(t *testing.T) {
	c := New("test-project", model.CredentialRef{Name: "gcp-key-minter"})
	cc := testContext("r1") // its resolver knows only "test-gcp-credential"
	res, err := c.Resolve(context.Background(), cc)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused {
		t.Fatal("expected a refusal when the standing credential resolves to nothing")
	}
	if !strings.Contains(res.Reason, "gcp-key-minter") {
		t.Errorf("reason %q does not name the credential an operator has to set", res.Reason)
	}
}

func TestResolveRefusesWithNoResolverAtAll(t *testing.T) {
	c := New("test-project", model.CredentialRef{Name: "gcp-key-minter"})
	cc := testContext("r1")
	cc.Credentials = nil
	res, err := c.Resolve(context.Background(), cc)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Refused {
		t.Fatal("expected a refusal rather than a panic when no resolver was wired up")
	}
}

// --- Materialize -----------------------------------------------------------

func TestMaterializePlacesTheKeyOutsideTheWorkspace(t *testing.T) {
	fm := newFakeMinter()
	c := testCapability(fm)
	cc := testContext("run-42")

	m, err := c.Materialize(context.Background(), cc)
	if err != nil {
		t.Fatal(err)
	}

	if len(m.Placements) != 1 {
		t.Fatalf("got %d placements, want 1: %+v", len(m.Placements), m.Placements)
	}
	p := m.Placements[0]
	if p.Side != model.SideSandbox {
		t.Errorf("Side = %v, want sandbox", p.Side)
	}
	if p.Path != KeyPath {
		t.Errorf("Path = %q, want %q", p.Path, KeyPath)
	}
	if p.EffectiveMode() != "600" {
		t.Errorf("EffectiveMode() = %q, want 600", p.EffectiveMode())
	}
	if len(fm.keys) != 1 {
		t.Fatalf("expected exactly one key minted, got %d", len(fm.keys))
	}
	for name, want := range fm.strings {
		if p.Content != want {
			t.Errorf("placement content = %q, want the minted key string %q for %s", p.Content, want, name)
		}
	}
}

func TestMaterializeNamesTheKeyAfterTheRun(t *testing.T) {
	fm := newFakeMinter()
	c := testCapability(fm)
	cc := testContext("run-77")

	if _, err := c.Materialize(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	var gotDisplayName string
	for _, k := range fm.keys {
		gotDisplayName = k.DisplayName
	}
	if want := "grain-run-77"; gotDisplayName != want {
		t.Errorf("display name = %q, want %q", gotDisplayName, want)
	}
}

func TestMaterializeReturnsALeaseWithExpiry(t *testing.T) {
	fm := newFakeMinter()
	c := testCapability(fm)
	cc := testContext("run-1")

	m, err := c.Materialize(context.Background(), cc)
	if err != nil {
		t.Fatal(err)
	}
	if m.Lease == nil {
		t.Fatal("expected a Lease")
	}
	if m.Lease.Capability != "gemini-key" {
		t.Errorf("Lease.Capability = %q, want gemini-key", m.Lease.Capability)
	}
	if m.Lease.MintedBy.Name != "test-gcp-credential" {
		t.Errorf("Lease.MintedBy = %+v, want the standing credential", m.Lease.MintedBy)
	}
	if m.Lease.ExpiresAt == nil || !m.Lease.ExpiresAt.Equal(cc.Now.Add(24*time.Hour)) {
		t.Errorf("Lease.ExpiresAt = %v, want %v", m.Lease.ExpiresAt, cc.Now.Add(24*time.Hour))
	}
	if m.Lease.Resource == "" {
		t.Error("Lease.Resource should name the minted key, for Revoke to delete it by later")
	}
	if m.CredentialOverride != nil {
		t.Error("a MINT capability should not set a credential override")
	}
}

func TestMaterializeFailurePropagatesAndMintsNoLease(t *testing.T) {
	fm := newFakeMinter()
	fm.createErr = errors.New("quota exceeded")
	c := testCapability(fm)

	m, err := c.Materialize(context.Background(), testContext("run-1"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if m.Lease != nil || len(m.Placements) != 0 {
		t.Fatalf("a failed Materialize must return nothing to apply, got %+v", m)
	}
}

// --- PromptSection -----------------------------------------------------

func TestPromptSectionNamesThePathNeverTheValue(t *testing.T) {
	c := New("test-project", model.CredentialRef{Name: "cred"})
	text, err := c.PromptSection(context.Background(), testContext("run-1"), []model.Placement{
		{Side: model.SideSandbox, Path: KeyPath, Content: "sekret-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !contains(text, KeyPath) {
		t.Errorf("prompt section should name %s, got %q", KeyPath, text)
	}
	if contains(text, "sekret-value") {
		t.Fatalf("prompt section leaked placement content: %q", text)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// --- Revoke --------------------------------------------------------------

func TestRevokeDeletesTheLeasedKey(t *testing.T) {
	fm := newFakeMinter()
	c := testCapability(fm)
	cc := testContext("run-1")

	m, err := c.Materialize(context.Background(), cc)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Revoke(context.Background(), cc, *m.Lease); err != nil {
		t.Fatal(err)
	}
	if len(fm.keys) != 0 {
		t.Errorf("expected the key to be gone, got %+v", fm.keys)
	}
	if len(fm.deleted) != 1 || fm.deleted[0] != m.Lease.Resource {
		t.Errorf("deleted = %v, want exactly %s", fm.deleted, m.Lease.Resource)
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	fm := newFakeMinter()
	c := testCapability(fm)
	cc := testContext("run-1")

	lease := model.Lease{Resource: "projects/p/locations/global/keys/already-gone"}
	if err := c.Revoke(context.Background(), cc, lease); err != nil {
		t.Fatalf("revoking a lease for a key that never existed should not error (fake minter): %v", err)
	}
}

func TestRevokePropagatesAnError(t *testing.T) {
	fm := newFakeMinter()
	fm.deleteErr = errors.New("api unreachable")
	c := testCapability(fm)

	err := c.Revoke(context.Background(), testContext("run-1"), model.Lease{Resource: "k"})
	if err == nil {
		t.Fatal("expected the underlying delete error to propagate")
	}
}

// --- DeleteExpired -------------------------------------------------------

func TestDeleteExpiredOnlyDeletesOldGrainKeys(t *testing.T) {
	fm := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm.keys = map[string]mintedKey{
		"old-grain":    {Name: "old-grain", DisplayName: "grain-task-1", CreateTime: now.Add(-25 * time.Hour)},
		"fresh-grain":  {Name: "fresh-grain", DisplayName: "grain-task-2", CreateTime: now.Add(-1 * time.Hour)},
		"old-other":    {Name: "old-other", DisplayName: "someone-elses-key", CreateTime: now.Add(-48 * time.Hour)},
		"exactly-edge": {Name: "exactly-edge", DisplayName: "grain-task-3", CreateTime: now.Add(-24 * time.Hour)},
	}

	deleted, err := deleteExpired(context.Background(), fm, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// "exactly-edge" sits exactly on the cutoff and is left alone --
	// only strictly older than maxAge is reaped, the same
	// created >= cutoff: continue boundary
	// grain/automation/gemini_keys.py's delete_expired_keys draws.
	want := map[string]bool{"old-grain": true}
	if len(deleted) != len(want) {
		t.Fatalf("deleted = %v, want %v", deleted, want)
	}
	for _, name := range deleted {
		if !want[name] {
			t.Errorf("deleted %s, which should have been left alone", name)
		}
	}
	for name := range want {
		if _, stillThere := fm.keys[name]; stillThere {
			t.Errorf("%s should have been deleted", name)
		}
	}
	for _, name := range []string{"fresh-grain", "old-other", "exactly-edge"} {
		if _, stillThere := fm.keys[name]; !stillThere {
			t.Errorf("%s should have been left alone", name)
		}
	}
}

func TestDeleteExpiredIsBestEffortPerKey(t *testing.T) {
	fm := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm.keys = map[string]mintedKey{
		"stuck":  {Name: "stuck", DisplayName: "grain-a", CreateTime: now.Add(-48 * time.Hour)},
		"normal": {Name: "normal", DisplayName: "grain-b", CreateTime: now.Add(-48 * time.Hour)},
	}

	// Wrap DeleteKey to fail once, for "stuck" only, without a full
	// second fake type.
	c := &failOnceMinter{fakeMinter: fm, failFor: "stuck"}

	deleted, err := deleteExpired(context.Background(), c, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "normal" {
		t.Fatalf("deleted = %v, want exactly [normal]; one bad key must not stop the rest", deleted)
	}
}

type failOnceMinter struct {
	*fakeMinter
	failFor string
}

func (f *failOnceMinter) DeleteKey(ctx context.Context, name string) error {
	if name == f.failFor {
		return errors.New("boom")
	}
	return f.fakeMinter.DeleteKey(ctx, name)
}

// --- the daemon's own operating key --------------------------------------

// The operating key carries displayNamePrefix like every other key this
// package mints, so the prefix check alone would reap it -- and it is
// older than any cutoff by design, being minted once at deploy time.
// Reaping it would stop the running daemon roughly a day after every
// deploy, which is the whole reason for the exemption.
func TestDeleteExpiredNeverReapsTheOperatingKey(t *testing.T) {
	fm := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm.keys = map[string]mintedKey{
		"operating": {Name: "operating", DisplayName: OperatingKeyDisplayName, CreateTime: now.Add(-90 * 24 * time.Hour)},
		"old-task":  {Name: "old-task", DisplayName: "grain-task-1", CreateTime: now.Add(-25 * time.Hour)},
	}

	deleted, err := deleteExpired(context.Background(), fm, now, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "old-task" {
		t.Fatalf("deleted = %v, want exactly [old-task]", deleted)
	}
	if _, stillThere := fm.keys["operating"]; !stillThere {
		t.Error("the daemon's operating key was reaped; it must outlive maxLease")
	}
}

// The exemption is by exact name, so the constant has to keep the prefix
// that marks it as grain's at all -- a key without it is invisible to
// the reaper for a different and much weaker reason (it looks like
// someone else's), and DeleteExpired would then also stop protecting
// every other grain key if the prefix were ever dropped here.
func TestOperatingKeyNameKeepsTheGrainPrefix(t *testing.T) {
	if !strings.HasPrefix(OperatingKeyDisplayName, displayNamePrefix) {
		t.Errorf("OperatingKeyDisplayName = %q, want it to keep the %q prefix", OperatingKeyDisplayName, displayNamePrefix)
	}
}

func TestMintOperatingKeyNamesAndRestrictsTheKey(t *testing.T) {
	fm := newFakeMinter()
	name, key, err := mintOperatingKey(context.Background(), fm)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Error("no key string returned")
	}
	minted, ok := fm.keys[name]
	if !ok {
		t.Fatalf("CreateKey returned %q, which is not in the minter", name)
	}
	if minted.DisplayName != OperatingKeyDisplayName {
		t.Errorf("display name = %q, want %q", minted.DisplayName, OperatingKeyDisplayName)
	}
}

// A minted operating key must carry the same per-service restriction
// every per-task key does: a leaked key is then useless against
// anything else enabled in the project.
func TestMintOperatingKeyRestrictsToTheGenerativeLanguageAPI(t *testing.T) {
	var gotTarget string
	fm := &recordingMinter{fakeMinter: newFakeMinter(), target: &gotTarget}
	if _, _, err := mintOperatingKey(context.Background(), fm); err != nil {
		t.Fatal(err)
	}
	if gotTarget != DefaultAPITargetService {
		t.Errorf("api target = %q, want %q", gotTarget, DefaultAPITargetService)
	}
}

type recordingMinter struct {
	*fakeMinter
	target *string
}

func (r *recordingMinter) CreateKey(ctx context.Context, displayName, apiTargetService string) (string, string, error) {
	*r.target = apiTargetService
	return r.fakeMinter.CreateKey(ctx, displayName, apiTargetService)
}

func TestMintOperatingKeyPropagatesAFailure(t *testing.T) {
	fm := newFakeMinter()
	fm.createErr = errors.New("apikeys is angry")
	if _, _, err := mintOperatingKey(context.Background(), fm); err == nil {
		t.Fatal("expected the underlying create error to propagate")
	}
}

// --- Reap ----------------------------------------------------------------

// Reap is the same sweep deleteExpired performs, reached with no task and
// no CapabilityContext in hand -- the shape cmd/grain's hourly
// reapCapabilities can call, and the reason a key minted for a run whose
// controller died before the lease was written is deleted at all.
func TestReapDeletesOnlyKeysOlderThanTheMaxLease(t *testing.T) {
	fm := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm.keys = map[string]mintedKey{
		"leaked":    {Name: "leaked", DisplayName: "grain-run-1", CreateTime: now.Add(-maxLease - time.Hour)},
		"live":      {Name: "live", DisplayName: "grain-run-2", CreateTime: now.Add(-time.Hour)},
		"operating": {Name: "operating", DisplayName: OperatingKeyDisplayName, CreateTime: now.Add(-90 * 24 * time.Hour)},
		"theirs":    {Name: "theirs", DisplayName: "someone-elses-key", CreateTime: now.Add(-90 * 24 * time.Hour)},
	}
	c := testCapability(fm)

	deleted, err := c.Reap(context.Background(), fakeResolver{"test-gcp-credential": "the-minter-key-material"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "leaked" {
		t.Fatalf("deleted = %v, want exactly [leaked]", deleted)
	}
	for _, name := range []string{"live", "operating", "theirs"} {
		if _, stillThere := fm.keys[name]; !stillThere {
			t.Errorf("%s should have been left alone", name)
		}
	}
}

// A half-wired deployment has no project to list and nothing minted to
// reap -- the same configuration Resolve refuses a task for. Reaping it
// must be a no-op rather than an hourly error in the daemon log, and
// must not reach the API Keys API at all.
func TestReapDoesNothingWhenUnconfigured(t *testing.T) {
	fm := newFakeMinter()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	fm.listErr = errors.New("ListKeys should never have been called")
	factory := func(ctx context.Context, credentialJSON, projectID string) (minter, error) {
		return fm, nil
	}
	creds := fakeResolver{"test-gcp-credential": "the-minter-key-material"}

	for _, tc := range []struct {
		name string
		c    *Capability
	}{
		{"no project", &Capability{Credential: model.CredentialRef{Name: "test-gcp-credential"}, factory: factory}},
		{"no credential", &Capability{ProjectID: "test-project", factory: factory}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deleted, err := tc.c.Reap(context.Background(), creds, now)
			if err != nil || deleted != nil {
				t.Fatalf("Reap = %v, %v, want nil, nil", deleted, err)
			}
		})
	}
}

// A resolver that cannot produce the minter credential is an error the
// sweep's caller logs, not a silent "nothing to reap": the keys are
// still there, and reporting none deleted would say they were checked.
func TestReapReportsAnUnresolvableCredential(t *testing.T) {
	c := testCapability(newFakeMinter())
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

	if _, err := c.Reap(context.Background(), fakeResolver{}, now); err == nil {
		t.Fatal("expected an error when the minter credential cannot be resolved")
	}
	// And a caller with no resolver at all is the same kind of failure,
	// rather than a nil-map panic inside the sweep.
	if _, err := c.Reap(context.Background(), nil, now); err == nil {
		t.Fatal("expected an error when there is no credential resolver")
	}
}

func TestReapReportsAFailedListing(t *testing.T) {
	fm := newFakeMinter()
	fm.listErr = errors.New("network error")
	c := testCapability(fm)

	_, err := c.Reap(context.Background(), fakeResolver{"test-gcp-credential": "the-minter-key-material"}, time.Now())
	if err == nil {
		t.Fatal("expected the underlying listing error to propagate")
	}
}

// The interface is the whole point of this method existing: cmd/grain's
// reapCapabilities finds a provider to sweep by type-asserting it, so a
// signature that drifts from model.Reaper's would leave gemini-key
// silently unswept again rather than failing to compile.
func TestCapabilitySatisfiesReaper(t *testing.T) {
	var _ model.Reaper = (*Capability)(nil)
}
