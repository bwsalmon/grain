package grain

import (
	"fmt"
	"path"
	"strings"
)

// Where a grain's material is mounted in its container. Everything with
// a shape -- a script, a file with a mode, a tree of them -- arrives as
// files rather than as environment variables, and only scalars the shim
// reads directly stay in the environment (env.go).
//
// The line is worth drawing because of what it opens. On Kubernetes a
// Secret or a ConfigMap mounted as a volume *is* this model already:
// `items: [{key, path, mode}]` gives files at chosen paths with chosen
// modes, injected by the kubelet before the container starts, with the
// pod spec holding only a reference. So placements need no encoding, no
// size ceiling that ARG_MAX imposes, and no appearance in
// /proc/1/environ at all -- and a non-secret placement (a CA bundle, a
// config template) can come from a ConfigMap instead, which an
// environment blob could not distinguish.
//
// Under docker the same tree is populated however that backend does it;
// the shim's contract is the tree, not how it was filled.
const (
	// Root is the directory a grain's material is mounted under.
	Root = "/grain"
	// FileCredential holds what the agent authenticates to its model API
	// with. A file rather than an environment variable so the container's
	// own environment carries no material at all: the profile reads it
	// and hands it to its CLI however that CLI wants it, which is often
	// an environment variable on the *child* process and is the profile's
	// business either way.
	FileCredential = Root + "/credential"
	// FilePrompt is the agent's opening prompt, assembled by the
	// controller from its store -- the task, its conversation, the
	// deployment's and the repo's prompt extensions, what earlier
	// attempts did.
	//
	// A file rather than an environment variable for the reason the setup
	// script is one: it is long and multi-line, and a variable would have
	// it survive whatever quoting a pod spec or a "docker run" applies.
	//
	// It is here at create rather than delivered once the sandbox is up,
	// which is what retired the two-phase start. That existed because the
	// prompt names what earlier attempts pushed and those commits were
	// read from the checkout -- but the controller can ask GitHub for a
	// branch's commits, and it already talks to GitHub every cycle. What
	// the two-phase start was really protecting is untouched: Setup's own
	// exit code still gates starting the agent, so a failed checkout
	// still spends no model tokens.
	FilePrompt = Root + "/prompt"
	// FileSetup is the setup script, run in the guest before the agent
	// starts.
	//
	// A file rather than an environment variable, though kontur's own
	// CHV_SETUP_SCRIPT carries its script's text: grain's is composed by
	// the controller out of a clone, a branch checkout, the repo's own
	// setup command and whatever the prompt needs read back, so it is
	// multi-line and substantial where kontur's is a line or two. As a
	// file it is a script -- a shebang, an executable bit, something a
	// human can cat in an incident -- rather than a string that has to
	// survive shell quoting on its way through a runtime's env handling.
	FileSetup = Root + "/setup"
	// DirPlacements holds the files to copy into the guest, in a tree
	// that mirrors their guest paths: a placement at /home/agent/.netrc
	// is mounted at /grain/placements/home/agent/.netrc.
	//
	// The tree *is* the mapping, so nothing carries a manifest beside it
	// and a Secret volume's own `items[].path` says where a key lands in
	// the guest without grain interpreting anything.
	DirPlacements = Root + "/placements"
)

// GuestActivityFile is the one path in this file that is a *guest* path
// rather than a container one: everything above is mounted into the
// container and copied inward, and this is written from inside the
// sandbox and read outward.
//
// It is how anything running in the guest sets Status.Activity. The
// agent has a cheaper route -- the status tool, a local file write in the
// container with no vsock hop -- so this exists for the two cases that
// route cannot serve:
//
//   - Setup, which runs before there is an agent at all. Activity's own
//     example is "cloning acme/widgets", which happens here; without this
//     the phrase was unreachable and PhaseProvisioning could only say
//     that a grain was provisioning, not what it had got to. A run killed
//     by ProvisionBudget is exactly the one that needs the difference.
//   - A long guest command the agent started and is waiting on, which can
//     report its own progress rather than leaving the last thing the
//     agent said to go stale for the length of a build.
//
// Read on the round trip the shim already makes for GuestHealth, so it
// costs nothing extra and inherits that cadence: a heartbeat, not a
// stream. Advisory by construction -- last writer wins, a torn read is a
// garbled phrase and nothing more -- which is what lets it be a plain
// file rather than a channel needing a protocol.
//
// A tool in the guest image writes it atomically; a setup script with no
// such tool can echo into it, and the file is the contract either way.
const GuestActivityFile = "/run/grain/activity"

// FileTerminationLog is where a grain writes its final Result on the way
// out, in addition to its status.
//
// Kubernetes surfaces this file's contents in the pod's own status --
// .status.containerStatuses[].state.terminated.message -- so a finished
// grain's outcome arrives in the same listing that enumerates it, with no
// exec at all. That matters for the one read that must not be missed: a
// grain that finished but whose status exec fails is a run the controller
// cannot finish, and it holds a slot until something notices.
//
// The path is Kubernetes' own default and its cap is a few kilobytes, so
// a Result belongs here and a trajectory does not. Pair it with
// terminationMessagePolicy: FallbackToLogsOnError for the shim that died
// before it could write one. Under docker nothing reads this file, and
// writing it anyway costs a few hundred bytes.
const FileTerminationLog = "/dev/termination-log"

// ModeSetup is the mode FileSetup is written with. Executable, because a
// script that has to be invoked through an interpreter cannot carry its
// own shebang, and the controller composing it should be free to choose
// one.
const ModeSetup = "0755"

// File is one piece of material and the mode it must be created with.
type File struct {
	Content string
	Mode    string
}

// Files renders everything this Spec delivers as files, keyed by the
// container path each lands at. A backend mounts or writes them; nothing
// here says which.
//
// A placement with an unusable path is an error rather than something
// skipped or sanitised: it means the controller composed something wrong,
// and writing three of four credentials is a worse outcome than writing
// none and saying so.
func (s Spec) Files() (map[string]File, error) {
	out := make(map[string]File, len(s.Placements)+3)
	if s.Framework.Credential != "" {
		out[FileCredential] = File{Content: s.Framework.Credential, Mode: "0600"}
	}
	if s.Prompt != "" {
		out[FilePrompt] = File{Content: s.Prompt, Mode: "0644"}
	}
	if s.Setup != "" {
		out[FileSetup] = File{Content: s.Setup, Mode: ModeSetup}
	}
	for _, p := range s.Placements {
		at, err := PlacementPath(p.Path)
		if err != nil {
			return nil, err
		}
		if _, clash := out[at]; clash {
			return nil, fmt.Errorf("grain: two placements both land at %s", p.Path)
		}
		out[at] = File{Content: p.Content, Mode: p.EffectiveMode()}
	}
	return out, nil
}

// EffectiveMode is Mode, defaulting to "0600" -- the safe answer, so a
// placement that leaves it unset does not thereby get a wider one. It
// mirrors model.Placement.EffectiveMode, whose own default this is.
func (p Placement) EffectiveMode() string {
	if p.Mode == "" {
		return "0600"
	}
	return p.Mode
}

// PlacementPath maps a guest path to the container path its material is
// mounted at.
//
// It refuses anything that is not already an absolute, cleaned path.
// That is a containment check, not tidiness: a placement path is the one
// part of this contract that names a location, and "/a/../../etc/shadow"
// under DirPlacements escapes the tree entirely. Refusing here means a
// controller cannot write outside the tree even by accident, and the shim
// gets to trust what it walks.
func PlacementPath(guest string) (string, error) {
	if guest == "" {
		return "", fmt.Errorf("grain: a placement has no path")
	}
	if !path.IsAbs(guest) {
		return "", fmt.Errorf("grain: placement path %q is not absolute", guest)
	}
	if cleaned := path.Clean(guest); cleaned != guest {
		return "", fmt.Errorf("grain: placement path %q is not in its simplest form (%q)", guest, cleaned)
	}
	return DirPlacements + guest, nil
}

// GuestPath is PlacementPath's inverse: where in the guest a file found
// under DirPlacements belongs. It is what the shim walks the tree with.
func GuestPath(container string) (string, error) {
	rest, ok := strings.CutPrefix(container, DirPlacements+"/")
	if !ok {
		return "", fmt.Errorf("grain: %q is not under %s", container, DirPlacements)
	}
	guest := "/" + rest
	// Validated on the way back too, not only on the way in: the shim
	// walks a directory somebody else mounted, and a symlink or a stray
	// file in it is not something PlacementPath ever saw.
	if _, err := PlacementPath(guest); err != nil {
		return "", err
	}
	return guest, nil
}
