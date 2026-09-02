# The image a v2 deployment actually runs: `grain` and every binary it
# shells out to, in one artifact CI publishes to GHCR on every commit
# (../.github/workflows/build-artifacts.yml) and scripts/setup.sh pulls
# rather than builds -- bwsalmon/agents#645.
#
# This is a *runtime* image, and the opposite of Dockerfile.build next to
# it. That one is a toolchain: it COPYs nothing, the working tree arrives
# as a bind mount, and what comes out is a binary the host then runs
# under its own systemd unit with whatever else the host happens to have
# installed. This one COPYs the whole checkout, builds inside itself, and
# what comes out is the deployment -- so "whatever else the host happens
# to have installed" stops mattering.
#
# What "all of its dependencies" means concretely, and why each is here:
#
#   git               the orchestrator clones every task's repo through
#                     the git proxy, and pkg/mcp hands each sandbox a
#                     credential helper to use with it
#   bash              pkg/mcp's run_command and pkg/capability/selfrepair
#                     both run `bash -c` for a task's own commands
#   openssh-client    KonturSandboxes reaches each guest over SSH
#   ca-certificates   every HTTPS call: GitHub's API, the agent CLIs
#   curl              the agent CLIs' own network calls, and a shell in
#                     here that needs to reach something
#   systemd           for `journalctl` alone -- pkg/systemlog.Journalctl
#                     shells out to it for the UI's Logs pane, against
#                     the host journal scripts/setup.sh mounts in (see
#                     that script's own docker_run_args)
#   docker CLI        pkg/kontur talks to the *host's* docker daemon over
#                     the socket setup.sh mounts, to inspect the
#                     container each sandbox VM runs in, and pkg/mcp's
#                     docker-exec transport reaches into it the same way
#   konturctl         pkg/kontur runs it to create, exec into and destroy
#                     those VMs. Built here from third_party/kontur, the
#                     same vendored copy the sandbox image is built from,
#                     so the two cannot drift apart in a deployment
#   claude, agy       the two agent CLIs a dispatch actually runs:
#                     agent/claude execs the Claude Code CLI,
#                     agent/antigravity execs Google's Antigravity CLI.
#                     Both frameworks are a live, per-task choice
#                     (README.md, "Two agent frameworks, either per
#                     task"), so an image carrying one of them is an
#                     image that fails every run choosing the other --
#                     they belong here together or not at all.
#
#                     Installed here rather than by setup.sh's own
#                     install_claude_cli, which this replaces: a CLI
#                     baked into the image has its presence settled at
#                     build time, in CI, instead of depending on every
#                     deployed host being able to reach claude.ai and
#                     antigravity.google at deploy time -- and on
#                     depending on it again on every re-deploy, forever.
#
# Build it from the *repository root*:
#
#     docker build -f Dockerfile -t grain .
#
# third_party/kontur is outside the Go tree and konturctl is built from
# it, and `go build` reads the commit stamp out of the root .git. `make
# image` (the Makefile's own target) already passes both.

# Kept in step with go.mod by the Makefile, which reads the version out
# of it and passes it as a --build-arg; the default here is what a bare
# `docker build` with no --build-arg gets, so it is written down twice on
# purpose and GOTOOLCHAIN=local below turns a stale copy into an error
# naming both versions rather than a silently different compiler.
ARG GO_VERSION=1.26.2
# Separate from the tag so a mirror can be substituted whole
# (GO_IMAGE=mirror.gcr.io/library/golang) on a network that does not
# reach Docker Hub -- the same escape hatch Dockerfile.build and the
# Makefile already give the toolchain image.
ARG GO_IMAGE=golang
ARG RUNTIME_IMAGE=debian:bookworm-slim
# The docker CLI is copied out of this image rather than apt-installed:
# Debian's own `docker.io` package pulls in the whole daemon --
# containerd, runc, iptables, a systemd unit -- to get one client binary
# that only ever talks to a socket mounted from the host.
ARG DOCKER_CLI_IMAGE=docker:28-cli

FROM ${DOCKER_CLI_IMAGE} AS docker-cli

FROM ${GO_IMAGE}:${GO_VERSION}-bookworm AS build

# See Dockerfile.build's own comment: a go.mod toolchain line newer than
# the pinned image would otherwise quietly fetch a different Go at build
# time, undoing the pin.
ENV GOTOOLCHAIN=local

# pkg/ui/frontend is React+Vite and `make build` runs `npm ci && npm run
# build` there before `go build`, so this stage carries a JS toolchain
# for the same reason Dockerfile.build does -- and, as there, Debian 12's
# own nodejs/npm are recent enough for the pinned Vite version, with no
# NodeSource repo to add.
RUN apt-get update && apt-get install -y --no-install-recommends nodejs npm \
	&& rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .

# Plain `make build`, out of the same Makefile a laptop runs, rather than
# an open-coded `go build` here: one copy of the build rules, so the
# binary CI publishes and the binary a developer builds cannot drift.
#
# BUILDVCS is an ARG so a context without a usable .git (an export, a
# `docker build` fed a tarball) can turn the commit stamp off with
# --build-arg BUILDVCS=false instead of failing on `go build`'s "error
# obtaining VCS status" -- see the Makefile's own BUILDVCS comment.
ARG BUILDVCS=auto
# SANDBOX_IMAGE stamps in the sandbox container this build goes with
# (cmd/grain/sandboximage.go). Empty -- a bare `docker build` -- leaves
# the source default, the tag CI keeps pointed at main; CI passes the
# exact sha- tag of the sandbox image built from this same commit, so the
# two halves of a kontur deployment can never be two different commits.
ARG SANDBOX_IMAGE=
RUN make -C v2 build BUILDVCS=${BUILDVCS} SANDBOX_IMAGE=${SANDBOX_IMAGE}

# konturctl only -- not `kontur` or `kontur-mem-agent`, which run inside
# each sandbox VM's own container (the image
# ../.github/workflows/build-artifacts.yml publishes as kontur-sandbox)
# rather than next to the daemon. pkg/kontur shells out to exactly this
# one binary. CGO_ENABLED=0 for the same reason Makefile forces it:
# a genuinely static binary, with no libc coupling to the stage it was
# built in.
RUN mkdir -p /out && cd third_party/kontur && CGO_ENABLED=0 go build -o /out/konturctl ./cmd/konturctl

FROM ${RUNTIME_IMAGE}

# libcap2-bin is installed, used, and removed in this one layer: setcap
# below is the only thing that needs it, and leaving it in the image
# would hand a task's own `bash -c` a tool for handing out capabilities.
#
# systemd is here for /bin/journalctl and nothing else -- see this file's
# header. --no-install-recommends keeps it to the binaries and their
# libraries rather than dragging in a whole init the container never
# runs.
RUN apt-get update && apt-get install -y --no-install-recommends \
		bash \
		ca-certificates \
		curl \
		git \
		openssh-client \
		procps \
		systemd \
		tini \
	&& rm -rf /var/lib/apt/lists/*

COPY --from=docker-cli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=build /src/bin/grain /usr/local/bin/grain
COPY --from=build /out/konturctl /usr/local/bin/konturctl

# The UI/API binds -ui-addr, and scripts/setup.sh's own default for that
# is port 80 -- which an unprivileged process cannot bind. The unit
# setup.sh writes runs this container as the host's own unprivileged
# `grain` account (never root), so systemd's AmbientCapabilities, which
# is how the pre-container unit granted this, has no equivalent here: a
# non-root process in a container gets no capability from --cap-add
# alone. A file capability does exactly what is needed instead -- with
# CAP_NET_BIND_SERVICE in the container's bounding set (the unit passes
# --cap-add=NET_BIND_SERVICE) this binary, and only this binary, gains
# it; every other process in the container, a task's `bash -c` included,
# gets nothing.
RUN apt-get update && apt-get install -y --no-install-recommends libcap2-bin \
	&& setcap cap_net_bind_service=+ep /usr/local/bin/grain \
	&& apt-get purge -y libcap2-bin && apt-get autoremove -y \
	&& rm -rf /var/lib/apt/lists/*

# The Claude Code CLI, installed the way scripts/setup.sh's own
# install_claude_cli used to install it on the host: fetched to a file
# and then run, never `curl | bash` -- a pipeline's exit status is its
# last command's, so a 403 from claude.ai would otherwise "succeed" and
# leave an image whose claude framework fails at every dispatch.
#
# Unlike on a host, a failure here is fatal, and deliberately so: on a
# host, refusing to deploy over a blocked download was the worse
# outcome (a gemini deployment never needs this binary), but an image is
# built once, in CI, with network access -- a build that cannot fetch it
# should fail loudly and be re-run rather than ship an image that is
# quietly missing a framework the UI still offers.
#
# INSTALL_CLAUDE_CLI=0 opts out for a build on a network that cannot
# reach claude.ai at all; the resulting image runs everything except the
# claude framework, which fails at dispatch naming the missing binary.
#
# $HOME is where the installer puts things, so it goes somewhere fixed
# and world-readable rather than /root, which the unprivileged uid this
# container runs as cannot read. /usr/local/bin/claude is a symlink onto
# the default PATH so the daemon's own bare exec.LookPath("claude")
# finds it with nothing to configure.
ARG INSTALL_CLAUDE_CLI=1
RUN if [ "$INSTALL_CLAUDE_CLI" = "1" ]; then \
		set -eu; \
		mkdir -p /opt/claude; \
		curl -fsSL --max-time 120 https://claude.ai/install.sh -o /tmp/claude-install.sh; \
		test -s /tmp/claude-install.sh; \
		HOME=/opt/claude bash /tmp/claude-install.sh; \
		rm -f /tmp/claude-install.sh; \
		test -x /opt/claude/.local/bin/claude; \
		ln -sf /opt/claude/.local/bin/claude /usr/local/bin/claude; \
		chmod -R a+rX /opt/claude; \
	fi

# The Antigravity CLI, the same way and for the same reasons: fetched to
# a file rather than piped into a shell, installed under a fixed
# world-readable $HOME (its installer, like claude's, puts the binary in
# $HOME/.local/bin), symlinked onto the default PATH so the daemon's own
# bare exec.LookPath("agy") finds it, and fatal on failure so a build
# that could not fetch it is re-run rather than shipped.
#
# The installer verifies its own SHA512 of the binary it downloads, which
# is why there is no checksum written down here to go stale.
#
# Where it lands is *found* rather than assumed, unlike claude's above:
# that path (~/.local/bin) is one this repo has proven by building the
# image, and this one is documented only by Google, so a layout change on
# their side should move the symlink rather than fail the build for a
# binary that is actually there. `agy --version` at the end is what makes
# "found something named agy" mean "installed something that runs".
#
# This is what setup.sh's verify_agent_cli used to warn about on every
# deploy: the repo had no installer URL for agy, so the default agent
# framework's binary was the operator's to install by hand on every host,
# and a deployment could offer "antigravity" in Settings and fail every
# run that chose it. GRAIN_AGY_PATH still overrides this with a copy on
# the host, for a deployment pinning a particular version.
ARG INSTALL_AGY_CLI=1
RUN if [ "$INSTALL_AGY_CLI" = "1" ]; then \
		set -eu; \
		mkdir -p /opt/antigravity; \
		curl -fsSL --max-time 120 https://antigravity.google/cli/install.sh -o /tmp/agy-install.sh; \
		test -s /tmp/agy-install.sh; \
		HOME=/opt/antigravity bash /tmp/agy-install.sh; \
		rm -f /tmp/agy-install.sh; \
		agy="$(find /opt/antigravity -type f -name agy -perm -u+x | head -n1)"; \
		test -n "$agy"; \
		ln -sf "$agy" /usr/local/bin/agy; \
		chmod -R a+rX /opt/antigravity; \
		agy --version >/dev/null; \
	fi

# No USER of its own. The unit scripts/setup.sh writes always passes
# --user with the host's own grain uid:gid, because everything this
# process writes -- the store, the secrets database, each sandbox's
# working tree -- lands in a host directory bind-mounted in, and has to
# come out owned by the account that owns those directories on the host
# rather than by whatever uid this image happened to name.
#
# tini as PID 1: the daemon forks agent CLIs which fork MCP servers of
# their own (pkg/procgroup), and a container's PID 1 is the only reaper
# there is -- without one, every one of those that outlives its parent
# stays a zombie in this namespace for as long as the daemon runs.
#
# ENTRYPOINT is the binary and CMD its subcommand, so `docker run <image>
# schema-version` reads as running the CLI (which is exactly how
# setup.sh's own /usr/local/bin/grain wrapper and pkg/upgrade's image
# health check use it) while the unit passes `daemon` and its flags.
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/grain"]
CMD ["daemon"]
