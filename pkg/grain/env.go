package grain

import (
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
// Scalars only. Anything with a shape -- a script, a file with a mode, a
// tree of them -- arrives as a file instead (files.go), which is what
// lets a Kubernetes Secret or ConfigMap volume be the delivery mechanism
// unchanged. So no material travels here at all: the credential is a
// file, and so is every placement.
const (
	// EnvVersion is the wire format the controller wrote this
	// environment to -- Version's own doc comment for what a shim does
	// with one it does not know.
	EnvVersion = "GRAIN_WIRE_VERSION"
	// EnvFramework is the agent profile to run: Name from FrameworkSpec.
	EnvFramework = "GRAIN_FRAMEWORK"
	// EnvMaxRuntime bounds the agent, in time.Duration notation.
	EnvMaxRuntime = "GRAIN_MAX_RUNTIME"
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

// Env renders the scalar half of this Spec: the environment a grain
// container is created with. Its material half is Files. Zero-valued
// fields are left out entirely, so a shape nobody asked for lets kontur
// apply its own default rather than being handed a zero.
//
// Returned as a map rather than written anywhere, because who sets these
// differs by backend and none of it is this package's business: `docker
// run -e` under one, a pod spec's env (with valueFrom for the material)
// under the other.
func (s Spec) Env() map[string]string {
	env := map[string]string{EnvVersion: s.Version}
	if s.Framework.Name != "" {
		env[EnvFramework] = s.Framework.Name
	}
	if s.MaxRuntime != 0 {
		env[EnvMaxRuntime] = s.MaxRuntime.String()
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
	return env
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
// It returns only the scalar half. The credential, the setup script and
// the placements are files the shim reads from Root, not variables, so a
// Spec recovered from an environment carries none of them -- which also
// means an error from here can never quote material.
//
// Shape is deliberately not read back either. Those variables are
// kontur's, and a grain that parsed them would be a second opinion about
// numbers it does not act on.
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
		Framework: FrameworkSpec{Name: lookup(EnvFramework)},
	}
	if raw := lookup(EnvMaxRuntime); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return Spec{}, fmt.Errorf("grain: parsing %s=%q: %w", EnvMaxRuntime, raw, err)
		}
		s.MaxRuntime = Duration(d)
	}
	return s, nil
}

// Getenv is os.Getenv, for passing to SpecFromEnv.
func Getenv(name string) string { return os.Getenv(name) }
