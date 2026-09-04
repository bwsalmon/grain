package staterepo

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings is the one piece of grain's configuration that cannot live in
// the state repository, because it is what says where the state
// repository is.
//
// It is a small JSON file in the data directory rather than a row in the
// database for exactly that reason: everything else grain is configured
// with is now downstream of a repository this names. Keeping it to three
// fields is deliberate -- an operator editing it by hand should be able
// to read the whole thing at once, and a bootstrap that writes it is
// writing something a human can check.
type Settings struct {
	// Remote is the repository grain pushes state to, or "" for a
	// local-only install. The zero value is therefore the local-only
	// deployment, which is what makes "no configuration at all" a working
	// grain rather than a broken one.
	Remote string `json:"remote,omitempty"`
	// Branch is the branch state lives on; DefaultBranch when empty.
	Branch string `json:"branch,omitempty"`
	// TokenFile names a file holding the credential to push with, for a
	// deployment whose state repository is not covered by the GitHub
	// credential ladder under <data-dir>/secrets/github. Left empty, that
	// ladder is what authenticates the push.
	//
	// A path rather than the token itself: this file is not encrypted,
	// and a credential written into it would be a credential in
	// plaintext in the data directory with nothing marking it as one.
	TokenFile string `json:"tokenFile,omitempty"`
	// CheckImage is the container image the CI workflow grain installs in
	// the state repository runs `grain state check` from, and empty is
	// the image this build of grain is itself published as
	// (cmd/grain/grainimage.go), falling back to DefaultCheckImage when
	// nothing stamped one in. Empty is the right answer for almost every
	// deployment for that reason: the check refuses a dump stamped with a
	// schema it does not know, so a workflow pointed at any build but
	// this one fails every pull request for a reason that has nothing to
	// do with the change, and this one is the one grain can name without
	// being told.
	//
	// Set, it is a pin grain maintains rather than one it argues with: it
	// is what grain writes into the workflow, and what grain keeps that
	// file pointed at. The deployment with a registry mirror, or one that
	// deliberately checks its state against some other build, says so
	// here rather than by editing the workflow -- an edit to that file
	// stops grain touching the whole of it, image line included.
	CheckImage string `json:"checkImage,omitempty"`
	// NoWorkflow stops grain installing that workflow at all -- the
	// operator whose state repository is checked by something else.
	//
	// It is here rather than expressed by deleting the file, because
	// grain writes the file back whenever it is missing (that is what
	// makes a merge that dropped it recoverable), so deleting it is not a
	// decision that stays made. Editing it is: a workflow that is already
	// there is never rewritten, so the operator who only wants a
	// different image or a different runner changes the file and leaves
	// this alone.
	NoWorkflow bool `json:"noWorkflow,omitempty"`
}

// SettingsFileName is what the file is called in the data directory.
const SettingsFileName = "state-repo.json"

// LoadSettings reads the settings beside dataDir. A file that is not
// there is not an error and not a prompt: it is the local-only
// deployment, and returning the zero value is what lets `grain daemon`
// start on a machine nobody has configured.
func LoadSettings(dataDir string) (Settings, error) {
	path := filepath.Join(dataDir, SettingsFileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("staterepo: reading %s: %w", path, err)
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return Settings{}, fmt.Errorf("staterepo: %s is not valid JSON: %w", path, err)
	}
	return s, nil
}

// SaveSettings writes the settings, atomically so a daemon reading them
// at the same moment sees one version or the other and never half a
// file.
func SaveSettings(dataDir string, s Settings) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("staterepo: preparing %s: %w", dataDir, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(dataDir, SettingsFileName)
	tmp, err := os.CreateTemp(dataDir, ".state-repo-*")
	if err != nil {
		return fmt.Errorf("staterepo: creating a temporary file next to %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
