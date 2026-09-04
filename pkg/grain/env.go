package grain

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// The environment a grain is configured by. Everything the controller
// declares reaches the container this way, which is kontur's own
// convention -- its README's "configured entirely from environment
// variables so it can be driven directly from a Kubernetes pod spec" --
// and the reason there is no `grain configure` step: a container starts
// knowing what it is, so there is no window between create and configure
// for a failure to fall into, and no "not configured yet" for a poll to
// mean.
//
// Material travels here too, credential and placements alike. An earlier
// draft delivered those over an exec's stdin on the grounds that an
// environment variable shows up in a Kubernetes pod spec, and that was
// simply wrong about how Kubernetes does this: `valueFrom.secretKeyRef`
// puts a *reference* in the pod spec, while the value lives in a Secret
// with its own RBAC -- `get secrets` being a distinctly more privileged
// verb than `get pods` -- and its own encryption at rest. The kubelet
// injects it at start, so it reaches /proc/1/environ inside the
// container, which is the side of the vsock boundary that is trusted
// anyway.
//
// Under docker the same variables are set directly, where the exposure
// argument was always weak: reading them needs the docker socket, which
// is root-equivalent on that host and can read the process's memory in
// any case.
const (
	// EnvVersion is the wire format the controller wrote this
	// environment to -- Version's own doc comment for what a shim does
	// with one it does not know.
	EnvVersion = "GRAIN_WIRE_VERSION"
	// EnvFramework is the agent profile to run: Name from FrameworkSpec.
	EnvFramework = "GRAIN_FRAMEWORK"
	// EnvCredential is what that agent authenticates to its model API
	// with. Its own variable rather than a key inside EnvPlacements so
	// that a Kubernetes deployment can point it at its own Secret key,
	// rotated and scoped on its own.
	EnvCredential = "GRAIN_CREDENTIAL"
	// EnvSetup is the script to run in the guest before the agent starts.
	// kontur's own CHV_SETUP_SCRIPT is the precedent for a script
	// arriving this way. It must carry no credential -- see Spec.Setup.
	EnvSetup = "GRAIN_SETUP"
	// EnvMaxRuntime bounds the agent, in time.Duration notation.
	EnvMaxRuntime = "GRAIN_MAX_RUNTIME"
	// EnvPlacements is the placements as one JSON array, rather than an
	// enumerated GRAIN_PLACEMENT_0_PATH family: a list of objects has no
	// good flat spelling, kontur has no precedent for one, and a single
	// variable is what a Kubernetes Secret key holds naturally.
	EnvPlacements = "GRAIN_PLACEMENTS"
)

// kontur's own variables, which grain sets and never reads. A grain's
// Shape is not interpreted here at all -- the shim starts the VMM as a
// child and kontur reads these itself (bwsalmon/kontur's
// internal/config), so the numbers pass straight through in kontur's
// vocabulary rather than grain's.
const (
	envKonturCPUs       = "CHV_CPUS"
	envKonturMemoryMB   = "CHV_MEMORY_MB"
	envKonturDiskSizeMB = "CHV_DISK_SIZE_MB"
)

// Env renders this Spec as the environment a grain container is created
// with. Zero-valued fields are left out entirely, so a shape nobody asked
// for lets kontur apply its own default rather than being handed a zero.
//
// Returned as a map rather than written anywhere, because who sets these
// differs by backend and none of it is this package's business: `docker
// run -e` under one, a pod spec's env (with valueFrom for the material)
// under the other.
func (s Spec) Env() (map[string]string, error) {
	env := map[string]string{EnvVersion: s.Version}
	if s.Framework.Name != "" {
		env[EnvFramework] = s.Framework.Name
	}
	if s.Framework.Credential != "" {
		env[EnvCredential] = s.Framework.Credential
	}
	if s.Setup != "" {
		env[EnvSetup] = s.Setup
	}
	if s.MaxRuntime != 0 {
		env[EnvMaxRuntime] = s.MaxRuntime.String()
	}
	if len(s.Placements) > 0 {
		encoded, err := json.Marshal(s.Placements)
		if err != nil {
			return nil, fmt.Errorf("grain: encoding placements: %w", err)
		}
		env[EnvPlacements] = string(encoded)
	}
	if s.Shape.CPUs != 0 {
		env[envKonturCPUs] = strconv.Itoa(s.Shape.CPUs)
	}
	if s.Shape.MemoryMB != 0 {
		env[envKonturMemoryMB] = strconv.Itoa(s.Shape.MemoryMB)
	}
	if s.Shape.DiskGB != 0 {
		env[envKonturDiskSizeMB] = strconv.Itoa(s.Shape.DiskGB * 1024)
	}
	return env, nil
}

// SpecFromEnv reads back what Env wrote. lookup is os.Getenv's shape;
// pass Getenv for the real environment.
//
// It rejects a version it does not know rather than reading what it
// recognises and ignoring the rest, which is the whole point of there
// being a version: a shim that half-understands its environment starts an
// agent that does the wrong thing quietly, where one that refuses costs a
// single run and says exactly what disagreed.
//
// Shape is deliberately not read back. Those variables are kontur's, and
// a grain that parsed them would be a second opinion about numbers it
// does not act on.
func SpecFromEnv(lookup func(string) string) (Spec, error) {
	got := lookup(EnvVersion)
	if got == "" {
		return Spec{}, fmt.Errorf("grain: %s is not set: this process is not running as a configured grain", EnvVersion)
	}
	if got != Version {
		return Spec{}, fmt.Errorf("grain: this environment is written to wire version %q and this build speaks %q", got, Version)
	}
	s := Spec{
		Version:   got,
		Framework: FrameworkSpec{Name: lookup(EnvFramework), Credential: lookup(EnvCredential)},
		Setup:     lookup(EnvSetup),
	}
	if raw := lookup(EnvMaxRuntime); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Spec{}, fmt.Errorf("grain: parsing %s=%q: %w", EnvMaxRuntime, raw, err)
		}
		s.MaxRuntime = Duration(d)
	}
	if raw := lookup(EnvPlacements); raw != "" {
		if err := json.Unmarshal([]byte(raw), &s.Placements); err != nil {
			// Never the value: it is material, and an unmarshal error
			// would quote it.
			return Spec{}, fmt.Errorf("grain: parsing %s (%d bytes): %w", EnvPlacements, len(raw), err)
		}
	}
	return s, nil
}

// Getenv is os.Getenv, for passing to SpecFromEnv.
func Getenv(name string) string { return os.Getenv(name) }
