package gcpkey

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	iam "google.golang.org/api/iam/v1"
	"google.golang.org/api/option"
)

// iamMinter is Minter against the real IAM API, holding one *iam.Service
// authenticated as whatever credential built it -- see NewIAMMinter.
type iamMinter struct {
	svc *iam.Service
}

// NewIAMMinter builds a Minter authenticated with credentialJSON -- a
// Google service-account credentials-file document, the same shape
// CapabilityContext.Credentials.Resolve returns for Config.MinterCredential
// or a Lease's own MintedBy. This is Provider's default NewMinter.
func NewIAMMinter(ctx context.Context, credentialJSON string) (Minter, error) {
	svc, err := iam.NewService(ctx, option.WithCredentialsJSON([]byte(credentialJSON)))
	if err != nil {
		return nil, fmt.Errorf("gcpkey: building an IAM client: %w", err)
	}
	return &iamMinter{svc: svc}, nil
}

// CreateKey mints a fresh key under account and decodes its private key
// material back out. PrivateKeyData arrives base64-encoded even when
// PrivateKeyType asks for TYPE_GOOGLE_CREDENTIALS_FILE -- IAM's own field
// doc says as much ("When base64 decoded, the private key data can be used
// to authenticate ... with gcloud auth activate-service-account") -- so the
// decoded bytes, not the raw field, are the credentials-file document
// Placement.Content should carry.
func (m *iamMinter) CreateKey(ctx context.Context, account string) (id, keyJSON string, err error) {
	key, err := m.svc.Projects.ServiceAccounts.Keys.Create(account, &iam.CreateServiceAccountKeyRequest{
		PrivateKeyType: "TYPE_GOOGLE_CREDENTIALS_FILE",
	}).Context(ctx).Do()
	if err != nil {
		return "", "", explainCreateFailure(ctx, m, account, err)
	}
	decoded, err := base64.StdEncoding.DecodeString(key.PrivateKeyData)
	if err != nil {
		return "", "", fmt.Errorf("gcpkey: the created key's private data did not decode: %w", err)
	}
	return keyID(key.Name), string(decoded), nil
}

// explainCreateFailure adds a sentence to the maximum-user-managed-keys
// case gcloud's own error text does not name -- see gcp_keys.py's
// _explain_create_failure, which the same bwsalmon/agents#140 postmortem
// (a per-retry key leak filled a project's key quota in minutes, and the
// raw "FAILED_PRECONDITION: Precondition check failed" gave no hint why)
// motivated there. Never worse than the original error: if listing keys to
// count them also fails, the original err is returned unwrapped.
func explainCreateFailure(ctx context.Context, m *iamMinter, account string, err error) error {
	detail := err.Error()
	if !strings.Contains(detail, "FAILED_PRECONDITION") && !strings.Contains(detail, "Precondition check failed") {
		return fmt.Errorf("gcpkey: minting a key for %s: %w", account, err)
	}
	keys, listErr := m.ListKeys(ctx, account)
	if listErr != nil {
		return fmt.Errorf("gcpkey: minting a key for %s: %w", account, err)
	}
	return fmt.Errorf(
		"gcpkey: minting a key for %s: %w\n\n"+
			"GCP allows at most 10 user-managed keys per service account, and %s "+
			"currently has %d. grain revokes a key when its task finishes and reaps "+
			"anything past its own max age, so hitting this cap means keys are being "+
			"created faster than either is releasing them.",
		account, err, account, len(keys),
	)
}

// DeleteKey revokes keyID under account. keyID is always the bare id --
// the last path segment of a key's own resource name -- never the full
// name, matching what Lease.Resource and KeyInfo.ID both carry.
func (m *iamMinter) DeleteKey(ctx context.Context, account, keyID string) error {
	_, err := m.svc.Projects.ServiceAccounts.Keys.Delete(account + "/keys/" + keyID).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("gcpkey: deleting key %s: %w", keyID, err)
	}
	return nil
}

// ListKeys lists every user-managed key under account -- KeyTypes(
// "USER_MANAGED") excludes the Google-managed keys every service account
// also has, which this package never touches (they cannot be listed,
// downloaded, or deleted by this API at all -- the same exclusion
// gcp_keys.py's own _list_keys applies with --managed-by=user).
func (m *iamMinter) ListKeys(ctx context.Context, account string) ([]KeyInfo, error) {
	resp, err := m.svc.Projects.ServiceAccounts.Keys.List(account).KeyTypes("USER_MANAGED").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gcpkey: listing keys for %s: %w", account, err)
	}
	out := make([]KeyInfo, 0, len(resp.Keys))
	for _, key := range resp.Keys {
		info := KeyInfo{ID: keyID(key.Name)}
		if key.ValidAfterTime != "" {
			if t, err := time.Parse(time.RFC3339, key.ValidAfterTime); err == nil {
				info.CreatedAt = t
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// keyID is the bare id off the end of a key's resource name --
// "projects/{p}/serviceAccounts/{e}/keys/{id}".
func keyID(name string) string {
	idx := strings.LastIndex(name, "/")
	if idx == -1 {
		return name
	}
	return name[idx+1:]
}
