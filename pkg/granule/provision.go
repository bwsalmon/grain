package granule

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
)

// Where granule puts its own two files in the guest. Both are granule's
// business rather than the controller's, which is why they are here and
// not in pkg/grain: the contract with the controller is the tree at
// Root and the records on stdout, and how a setup script comes to
// be runnable inside the sandbox is an implementation of that, not part
// of it.
const (
	// GuestClientPath is where the grain client lands. On PATH, because
	// the prompt tells an agent to run "grain open-pull-request" rather
	// than naming a path, and a prompt that named one would be a third
	// place the layout is written down.
	GuestClientPath = "/usr/local/bin/grain"
	// GuestSetupPath is where the controller's setup script lands.
	// Under /run rather than a home directory: it is granule's, not the
	// work's, and a repo that lists its own directory should not find
	// it.
	GuestSetupPath = "/run/grain/setup"
)

// ProvisionSource is one file granule pushes into the guest, and where
// it came from.
type ProvisionSource struct {
	// GuestPath is where it lands, absolute.
	GuestPath string
	// LocalPath is the container file to read it from.
	LocalPath string
	// Mode is the guest file's mode.
	Mode fs.FileMode
}

// ProvisionPlan is everything that goes into a guest before setup runs,
// in the order a tar should carry it.
//
// Built as a value rather than streamed straight out because it is worth
// being able to assert on: what lands in a sandbox, and with which
// modes, is the part of provisioning where a mistake is a leaked
// credential rather than a failed run.
type ProvisionPlan struct {
	Files []ProvisionSource
	// Setup is true when a setup script was found and is in Files, so a
	// caller knows whether to run one without stat'ing again.
	Setup bool
}

// PlanProvision reads the mounted tree at root -- Root in a real
// container -- and says what belongs in the guest.
//
// Placement paths are validated on the way back out (GuestPath),
// not merely on the way in. The controller checked them when it composed
// the Spec, but this walks a directory somebody else mounted: a symlink
// or a stray file in it is not something that check ever saw, and this
// is the last point before the bytes land in a sandbox.
func PlanProvision(root, clientBinary string) (ProvisionPlan, error) {
	var plan ProvisionPlan

	if clientBinary != "" {
		if _, err := os.Stat(clientBinary); err == nil {
			plan.Files = append(plan.Files, ProvisionSource{
				GuestPath: GuestClientPath, LocalPath: clientBinary, Mode: 0o755,
			})
		}
	}

	setup := path.Join(root, path.Base(FileSetup))
	if _, err := os.Stat(setup); err == nil {
		plan.Files = append(plan.Files, ProvisionSource{
			GuestPath: GuestSetupPath, LocalPath: setup, Mode: 0o700,
		})
		plan.Setup = true
	}

	dir := path.Join(root, path.Base(DirPlacements))
	entries, err := placementFiles(dir)
	if err != nil {
		return ProvisionPlan{}, err
	}
	for _, local := range entries {
		// The tree is the mapping, so the guest path is derived from
		// where the file sits rather than from a manifest beside it.
		rel, err := filepath.Rel(dir, local)
		if err != nil {
			return ProvisionPlan{}, fmt.Errorf("granule: %s is not under %s", local, dir)
		}
		guest, err := GuestPath(DirPlacements + "/" + filepath.ToSlash(rel))
		if err != nil {
			return ProvisionPlan{}, err
		}
		info, err := os.Lstat(local)
		if err != nil {
			return ProvisionPlan{}, fmt.Errorf("granule: reading placement %s: %w", guest, err)
		}
		// Regular files only, and a symlink is refused rather than
		// followed: a link in a mounted tree points wherever whoever
		// mounted it aimed it, and following one would copy a file the
		// controller never placed.
		if !info.Mode().IsRegular() {
			return ProvisionPlan{}, fmt.Errorf("granule: placement %s is not a regular file (%s)", guest, info.Mode())
		}
		plan.Files = append(plan.Files, ProvisionSource{
			GuestPath: guest, LocalPath: local, Mode: info.Mode().Perm(),
		})
	}
	return plan, nil
}

// placementFiles lists every regular file under dir, deepest path last
// and sorted, so a tar built from it is byte-identical run to run. An
// absent directory is not an error: a grain with no placements is
// ordinary.
func placementFiles(dir string) ([]string, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("granule: reading the placement tree at %s: %w", dir, err)
	}
	sort.Strings(out)
	return out, nil
}

// Tar renders a plan as a tar stream rooted at "/", which is what
// Guest.Unpack takes.
//
// Parent directories are emitted explicitly and 0755, because a
// placement at /home/agent/.netrc has to be able to create /home/agent
// in a guest that has no such account -- and because leaving it to the
// unpacker means the directory's mode is whatever that unpacker's umask
// happened to be, which is not a thing to leave to chance on the path a
// credential lands on.
func (p ProvisionPlan) Tar() ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	seen := map[string]bool{}

	for _, f := range p.Files {
		for _, dir := range parents(f.GuestPath) {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeDir,
				Name:     dir[1:] + "/",
				Mode:     0o755,
			}); err != nil {
				return nil, fmt.Errorf("granule: writing directory %s: %w", dir, err)
			}
		}
		body, err := os.ReadFile(f.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("granule: reading %s: %w", f.LocalPath, err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     f.GuestPath[1:],
			Mode:     int64(f.Mode),
			Size:     int64(len(body)),
		}); err != nil {
			return nil, fmt.Errorf("granule: writing %s: %w", f.GuestPath, err)
		}
		if _, err := tw.Write(body); err != nil {
			return nil, fmt.Errorf("granule: writing %s: %w", f.GuestPath, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("granule: closing the provisioning tar: %w", err)
	}
	return buf.Bytes(), nil
}

// parents lists the ancestors of an absolute path, shallowest first and
// excluding "/" itself.
func parents(p string) []string {
	var out []string
	for dir := path.Dir(p); dir != "/" && dir != "."; dir = path.Dir(dir) {
		out = append(out, dir)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ModeString renders a mode the way File writes one, for the
// diagnostics that quote a plan.
func ModeString(m fs.FileMode) string { return "0" + strconv.FormatUint(uint64(m.Perm()), 8) }
