# Bootstrap: collapsing setup to one command

`docs/runbook.md`'s first-time checklist is fourteen steps. This is the
design for getting it to **one command plus one interactive login per
sandbox**, and the reasoning for why that is possible without weakening
anything the credential model rests on.

Two of the fourteen are documented as irreducibly manual. Neither is. One
of them is blocked by a real bug in the current key handling, described
below — that bug is the reason setup feels irreducible rather than merely
unsequenced.

- [What the fourteen steps actually are](#what-the-fourteen-steps-actually-are)
- [The bug: one key path, two roles](#the-bug-one-key-path-two-roles)
- [The Claude credential](#the-claude-credential)
- [Design](#design)
  - [Phase 1 — key roles](#phase-1--key-roles)
  - [Phase 2 — the cluster is a file](#phase-2--the-cluster-is-a-file)
  - [Phase 3 — the three missing verbs](#phase-3--the-three-missing-verbs)
  - [Phase 4 — the sequencer](#phase-4--the-sequencer)
- [What must not break](#what-must-not-break)
- [Testing: what runs here, what needs the dev host](#testing-what-runs-here-what-needs-the-dev-host)
- [Deferred](#deferred)

## What the fourteen steps actually are

| Runbook step | Verdict |
|---|---|
| 1. Host baseline (KVM, packages) | scriptable; a preflight check plus `apt-get`, or the instance's own startup script |
| 2. `grain host up` | already one idempotent command |
| 3. Fetch base image | scriptable — fetch-on-miss |
| 4. Create controller | already one command |
| 5. **Copy controller pubkey to host** | documented as irreducible. **It is not** — see below |
| 6. Create sandboxes | already one command |
| 7. **Deploy code to `/opt/grain`** | documented as needing a deploy credential. **It does not** — the host already has the tree |
| 8. GitHub credential files | irreducible: a human pastes a secret, once |
| 9. `repo-allowlist.json` | derivable from the same `owner/repo` as step 11 |
| 10. `sandbox-tokens.json` | **already unnecessary.** `SandboxTokenStore.ensure_token()` mints and records one per sandbox, idempotently, on first dispatch. The runbook is stale |
| 11. `automation.json` | two fields with no default, `owner` and `repo` |
| 12. Enable proxy + timer | scriptable once its preconditions hold, which is the only reason provisioning leaves it undone |
| 13. Claude login per sandbox | **superseded, not just reduced to one** — see [The Claude credential](#the-claude-credential). No sandbox ever holds this credential now; `claude -p` runs on the controller as a dedicated `grain-agent` account instead |
| 14. Verify | should be the tail of whatever did the work |

So: **two credentials placed once, and one machine that must exist.**
Everything else is sequencing.

Step 7's premise was `git clone` from GitHub, which would indeed need a
credential. But the host is already running this code — the deployment is a
664 KB file copy from the machine that holds the tree, over the SSH path
that has to exist anyway. No credential is involved at all.

Step 5 is the interesting one.

## The bug: one key path, two roles

`LibvirtAdapter.create()` reads **one** public key from **one** path and
injects it into **every** VM:

```python
ssh_public_key = (
    self.ssh_public_key_path.read_text()          # /var/lib/grain/controller-ssh.pub
    if self.ssh_public_key_path.exists() else None
)
meta_data_path.write_text(render_meta_data(spec.name, ssh_public_key))
```

At controller-create time (step 4) that file does not exist yet — the
controller generates the keypair itself, at first boot, in
`provision/controller.sh`. So the controller comes up with **no authorized
key at all**, and step 5 then instructs:

```sh
ssh debian@10.100.0.2 cat /data/secrets/controller-ssh.pub | sudo tee /var/lib/grain/controller-ssh.pub
```

Nothing has granted that access. The only way the documented sequence works
is if the operator pre-places *their own* public key at
`/var/lib/grain/controller-ssh.pub` — which step 5 then overwrites with the
controller's. One path, two different keys, two different purposes, swapped
silently halfway through setup. Recreating the controller afterwards
re-injects the wrong key entirely.

That is why the step reads as irreducible: there is no non-interactive way
in, so a human with two terminals is the only thing that closes the loop.

**The fix is that `public-keys` is a list.** cloud-init's NoCloud meta-data
accepts many; `render_meta_data` happens to emit one. Split the roles:

| Key | Lives at | Injected into | Purpose |
|---|---|---|---|
| **admin** | `/var/lib/grain/admin-ssh.pub` | controller **and** sandboxes | host → any VM, for setup and repair |
| **controller** | `/var/lib/grain/controller-ssh.pub` | sandboxes only | the automation dispatch path |

`VmSpec` already carries `role`, so the adapter selects by role with no new
plumbing. With the admin key present from the start, the host can read the
controller's own key back the moment it boots — step 5 becomes a scripted
stage, not a human. As a side effect the operator can reach a sandbox
directly, which is what step 13's login needs and currently has to reach by
hopping through the controller.

## The Claude credential

**Superseded — `claude -p` does not run on a sandbox at all anymore, so
none of them ever hold this credential.** The section below described an
earlier design (one login's `~/.claude/.credentials.json` injected onto
every sandbox); `docs/design.md`'s "Final choice: no credential in the
sandbox at all" has the live finding that ended it — the credential leaked
into any unsandboxed Bash subprocess's environment trivially, and no
sandbox-side hardening could close that, because none of it was ever
guarding against an *environment variable* read.

The actual mechanism now: `claude -p` runs on the **controller**, as a
dedicated, unprivileged `grain-agent` account — never root, never the
account `grain-automation.service` itself runs as. Generate a token
dedicated to this deployment with `claude setup-token` (deliberately not
an operator's own `claude login` session, so dispatch traffic never rides
on a personal credential), and pass it to `host bootstrap
--claude-token-file <path>` (or `grain controller configure
--claude-token-file <path>` on its own). `configure_claude_token`
(`grain/automation/configure.py`) places it at two mode-600 locations: a
root-owned reference copy under `/data/secrets`, and a live copy owned by
`grain-agent` — the same stdin-not-argv SSH mechanism
`configure_git_credentials` already uses for the git-proxy token, applied
here too. `dispatch.py`'s own unit script reads the live copy into
`CLAUDE_CODE_OAUTH_TOKEN` at runtime, never as a `systemd-run` argument
(which would put it in `ps` output).

That token being a bare `setup-token` value rather than a full login
session (no refresh token, ~1 year lifetime) is what makes the earlier
"concurrent refresh" open question moot rather than merely resolved: there
is no refresh cycle for concurrent `claude -p` processes to race on.
`docs/roadmap.md` item 8's second "Update" has the full account of what
was tried and found live before landing on this design, including two
smaller bugs (a shared-file SSH-key permission problem, and a `--tools`
syntax gap) the first real end-to-end run surfaced and fixed.

## Design

Four phases, each independently landable, each green on its own.

### Phase 1 — key roles

- `render_meta_data(name, ssh_public_keys: Sequence[str] = ())` — emit one
  `public-keys` entry per key. Pure; fully unit-testable.
- `LibvirtAdapter.__init__` takes `admin_public_key_path` and
  `controller_public_key_path` instead of one `ssh_public_key_path`.
  `create()` selects on `spec.role`: `CONTROLLER` → `[admin]`,
  `SANDBOX` → `[admin, controller]`, skipping any that is absent.
- CLI: `--admin-ssh-public-key` and `--controller-ssh-public-key` replace
  `--ssh-public-key`. A clean rename, not an alias — there is no deployment
  to migrate, and keeping the old name pointed at one of two roles is
  exactly the ambiguity this removes.
- If the admin key is absent when bootstrap runs, generate an `ed25519`
  keypair at `/var/lib/grain/admin-ssh{,.pub}` and say so on stdout.
  Supplying your own (`--admin-ssh-public-key ~/.ssh/id_ed25519.pub`) stays
  the better habit; auto-generation is what makes a fresh host work with no
  prior setup.

This phase fixes a live bug and is worth landing on its own regardless of
whether the rest follows.

### Phase 2 — the cluster is a file

Bootstrap has to name the base image, and today it cannot: `cli.py` builds
`Cluster(sandbox_count=args.sandboxes)` and every other field — subnet,
bridge, image, CPU/memory/disk — is a source-code default with no override.
That is the `--image` gap the runbook already lists.

- `Cluster.load(path)` reading a small TOML file (`tomllib` is stdlib on
  3.11, so this costs no dependency), falling back to the dataclass
  defaults when the file is absent.
- Global `--cluster-file`, default `/var/lib/grain/cluster.toml`, plus a
  direct `--image` override.

```toml
sandbox_count = 2
image = "/var/lib/grain/images/debian-12.qcow2"
# subnet, bridge, and the per-role sizes keep their defaults unless set
```

The inventory stays the single source of truth — this only changes where
its values come from, not that everything else derives from them.

### Phase 3 — the three missing verbs

Each is a CLI verb in its own right, not a private helper, so it is usable
and testable standalone and the sequencer stays a composition rather than a
monolith.

**`grain host wait <name>`** — `_wait_for_ssh` then `cloud-init status
--wait`. Both already exist, written and live-verified, in
`tests/loadtest.py`; this lifts them into `grain/adapter/` where the CLI can
reach them.

**`grain host deploy [name]`** — `tar -cz` the working tree, pipe it over
SSH, extract to `/opt/grain`. Excludes `.git`, `docs/`, `__pycache__`. No
credential, no network egress, no `git` on the far side.

**`grain controller configure`** — writes the per-deployment data to
`/data`:

| Input | Written to |
|---|---|
| `--repo owner/name` | `/data/config/automation.json`, `/data/config/repo-allowlist.json` |
| `--github-token-file PATH \| -` | `/data/secrets/github/<name>.token`, `0600` |
| `--credential-name` (default `bot`) | `/data/secrets/github/credentials.json` |
| `--claude-token-file PATH` | `/data/secrets/claude-oauth-token`, `0600`, plus a live copy at `grain-agent`'s own `~/.claude-oauth-token` |

The token goes over SSH **stdin**, never argv and never user-data. This is
not a new mechanism: `dispatch.configure_git_credentials` already writes the
sandbox token this way (`dd of=… status=none` with the value on stdin), for
the same reason — argv lands in `ps` output and in this project's own
command logging. Reusing the shape rather than inventing a second one is
the convention `metadata/launcher.py` calls out by name.

User-data is specifically ruled out: it is baked into the seed ISO, which
sits on host disk at rest.

### Phase 4 — the sequencer

```sh
grain host bootstrap --repo owner/name \
    --github-token-file - \
    --claude-token-file ~/.claude-setup-token
```

**No state file.** Every stage converges from observed reality. This is the
same call the adapter already makes — `state()` and `list_vms()` exist
precisely because "having them guess would be worse" — and it keeps the
answer consistent with rejecting a Terraform state file for this layer: a
second record of what exists is a second thing that can disagree with the
inventory.

| # | Stage | Converge condition | Replaces |
|---|---|---|---|
| 1 | Preflight | `/dev/kvm`, required binaries, base image present → else fetch | 1, 3 |
| 2 | Admin key | key file absent → generate | — |
| 3 | Network | `network_up()`, already idempotent | 2 |
| 4 | Controller | `ABSENT` → create + start; `STOPPED` → start; `RUNNING` → skip | 4 |
| 5 | Wait | SSH answers, then `cloud-init status --wait` | — |
| 6 | Controller key | read back over SSH; write if changed | **5** |
| 7 | Deploy | push the tree to `/opt/grain` (unconditional; it is a sync) | **7** |
| 8 | Configure | write `/data` config; token only if one was supplied | 8, 9, 11 |
| 9 | Sandboxes | per sandbox: `ABSENT` → create with both keys; `STOPPED` → start. No Claude credential is ever injected here — stage 8 already placed it on the controller for `grain-agent`, and no sandbox needs one | 6 |
| 10 | Enable | `systemctl enable --now` proxy + timer | 12 |
| 11 | Verify | `automation status`, `github audit`, `host health` | 14 |

Stage 6 before stage 9 is the whole point: sandboxes cannot be created
until the controller's key exists to inject.

`create()` deliberately *"must fail rather than silently adopt an existing
one"*, so the sequencer tests `state()` and skips — it never calls `create`
speculatively and swallows the error. That distinction is what makes a
re-run after a failure at stage 7 resume rather than explode.

**Repairing a recreated controller.** A recreated controller generates a
*new* key at first boot, and the sandboxes still trust the old one — which
silently breaks every dispatch. Stage 6 detects the change; stage 9 then
appends the new key to each sandbox's `authorized_keys` over the **admin**
key rather than recreating anything. That repair is only possible because
of Phase 1: today there is no second way in. See [Deferred](#deferred) for
the deeper fix.

**Legibility.** `grain --dry-run host bootstrap …` prints every command and
runs none. The flag is already global, and it is what keeps a one-command
setup from being an opaque one — the same argument `grain host rules`
already makes for the firewall.

## What must not break

**"No secret is ever baked into an image or a provisioning script."**
Preserved, and Phase 3 is written to preserve it: the token travels over
SSH stdin to a `0600` file on the controller. It is never in user-data,
never in argv, never on host disk.

**"The host holds no secrets."** Two asterisks, both worth stating in
`design.md` rather than happening quietly:

- The GitHub token transits the host *process* on its way to the
  controller. Never host disk.
- The auto-generated admin private key sits on the host.

Neither grants the host a capability it lacks. It owns the hypervisor and
writes every VM's cloud-init; the threat model already reads *"a compromised
host owns the hypervisor and therefore every VM."* But "holds no secrets"
becomes "holds no *system* credentials — no GitHub token, no GCP key, no
sandbox token at rest," and the doc should say the narrower thing.

If that trade is unwanted, the variant is bootstrap stopping before stage 8
and printing the two commands to run on the controller. It costs one step
and keeps the host token-free.

**The inventory stays the single source of truth.** Nothing in this design
discovers an address, and Phase 2 moves where the inventory's values come
from without changing that everything derives from them.

## Testing: what runs here, what needs the dev host

Most of it does **not** need the dev VM. This repo's architecture is already
arranged for exactly this split — `Runner` behind a protocol with
`FakeRunner`, `render_*` as pure functions, `FakeAdapter` in
`tests/test_loadtest.py` — and the live suites skip themselves cleanly on a
machine that cannot run them (currently 27 skipped, 420 passed).

| Verifiable with no hypervisor | How |
|---|---|
| `render_meta_data` with 0/1/N keys | pure function |
| Role → key selection in `create()` | `FakeRunner`, assert the rendered meta-data per role |
| `Cluster.load` and `--image` override | pure |
| Sequencer stage order | `FakeRunner` + fake adapter; **assert the controller key is read before any sandbox is created** — the regression that matters most |
| Skip-if-present at every stage | fake adapter reporting `RUNNING`; assert no `create` |
| Resume after a mid-chain failure | fake raising at stage 7; re-run, assert stages 1–6 are skipped |
| Token never in argv | mirror `test_configure_git_credentials_writes_the_token_over_stdin_not_argv` — it already asserts the value appears in no argv element anywhere |
| Deploy tar excludes `.git` | assert on the recorded command |

| Needs the dev host | Why |
|---|---|
| boot → SSH → `cloud-init status --wait` → key read-back | the timing is the thing being tested |
| tar-over-ssh deploy against a real controller | `sudo`, paths, ownership |
| Full bootstrap, bare host → green `automation status` | the actual claim |
| Controller recreate → key repair → dispatch still works | the failure mode Phase 4 exists to handle |

New live coverage goes in `tests/test_bootstrap_integration.py`, gated by
the same `_host_ready()` `skipif` the other live suites use, so
`python3 -m pytest` stays safe to run anywhere.

This container has no `/dev/kvm` and no `virsh`, `qemu-img`, or
`cloud-localds` — so the live table is genuinely blocked here, and only the
live table. That matches this project's standing bar: the five dispatch
bugs in `design.md` all *"looked obviously fine on paper and only surfaced
by actually booting a guest."* A bootstrap sequencer is exactly the kind of
code where that is true again, which is why Phase 5 is a live run on the dev
host and a runbook rewrite, not a green unit suite.

## Where the code is

Every touch point this design implies, so implementation does not start
with a search:

| What | Where |
|---|---|
| One key, all VMs — **the bug** | `grain/adapter/libvirt.py:214-218` (in `create`) |
| Emits a single `public-keys` entry | `grain/adapter/libvirt.py:126` `render_meta_data` |
| The single key path, and its default | `grain/adapter/libvirt.py:178-194` `__init__` |
| `--ssh-public-key`, to split in two | `grain/cli.py:366` |
| `Cluster(sandbox_count=…)` — every other field a source default | `grain/cli.py:52` |
| `--sandboxes`, superseded by the cluster file | `grain/cli.py:362` |
| `VmSpec.role`, which the key selection keys off | `grain/inventory.py`, `Role` / `VmSpec` |
| `_wait_for_ssh`, `_wait_for_provisioning`, `booted_vms` — lift into `grain/adapter/` | `tests/loadtest.py:112`, `:129`, `:154` |
| stdin-not-argv secret write, the shape to reuse | `grain/automation/dispatch.py:217` `configure_git_credentials` |
| …and the test that asserts it, to mirror | `tests/test_automation_dispatch.py:46` |
| `_host_ready()` + `skipif`, the gate for new live tests | `tests/test_vm_integration.py:106`, `:124` |
| `ensure_token`, why runbook step 10 is already dead | `grain/proxy/tokens.py:74` |
| Claude credential deny rule — **removed**, no sandbox holds this credential anymore | superseded, see `docs/design.md`, "Final choice" |
| "Rotate on recreate", the claim that narrows | `docs/design.md:986` |
| Fourteen-step checklist, to rewrite last | `docs/runbook.md`, "First-time setup checklist" |
| Gap list that shrinks as phases land | `docs/runbook.md`, "Gaps" |

Docs to update when the phases land: `docs/runbook.md` (the checklist and
the gap list), `docs/design.md` (the narrowed "host holds no secrets"
claim), `docs/system-diagram.md` (its "Where the diagram is aspirational"
section names the `--image` gap), and this file's status in
`docs/roadmap.md`.

## Deferred

**`/data` on its own disk.** `design.md` says *"on GCP, `/data` is a
separate persistent disk so it survives rebuilding the controller VM
itself."* In the libvirt implementation it is not — `create()` makes one
qcow2, so a controller recreate destroys every credential along with the
key. The bootstrap repair above handles the *symptom*; the fix is a second
disk plus a mount unit in `provision/controller.sh`, roughly 20 lines, and
it makes the design's own prose true. Worth its own roadmap item rather
than being smuggled into this one.

**The controller-side LLM proxy.** No sandbox holds the Claude credential
anymore — that part of this proxy's original case is already done, by
moving `claude -p` itself off the sandbox rather than by adding a proxy
(see [The Claude credential](#the-claude-credential)). What the proxy would
still add: it is the only place agent spend can be metered and capped, and
it would remove the credential from the controller's `grain-agent` account
too, replacing a real usable token there with a scoped one — a further
reduction, not the same problem this design already closed.

~~**`grain sandbox login <name>`.**~~ Built alongside this design rather
than left deferred — direct interactive admin SSH to a sandbox or the
controller, over the admin key Phase 1 makes every VM trust. See
`docs/roadmap.md` item 11.
