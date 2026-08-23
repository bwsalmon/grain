# Next session: the one thing standing between here and a real agent PR

This session ran `grain host bootstrap` for real — real KVM VMs, a real
`CLAUDE_CODE_OAUTH_TOKEN` login, a real deployed `grain-automation.timer` —
against a small mock GitHub server standing in for the real API (new
`--github-host`/`--git-forward-host`/`--github-insecure-http` flags). Full
detail, including everything tried and ruled out, is in
[`docs/roadmap.md`](roadmap.md) item 8's "Update" section and
[`docs/design.md`](design.md)'s permission-mode note. This file is the
short version: what's actually left, and where to start.

## The blocker

A real dispatched `claude -p` clones and commits fine (the sandbox's own
`autoAllowBashIfSandboxed` covers that), but `git push origin
HEAD:<branch>` — the exact command `dispatch.py`'s prompt tells it to run —
needs a sandbox network-domain approval that a headless `-p` run has no way
to answer. Every real dispatch ends the same way: the unit exits zero
having never pushed, and the sweeper's own "succeeded but branch doesn't
exist" path (correctly) requeues it. Confirmed reproducible: 5 live
dispatch attempts, same result each time.

**Tried and ruled out, live, against a real login:**

- `sandbox.network.allowedDomains` set to the git proxy's address
  (`["10.100.0.2"]`), to `host:port` (`["10.100.0.2", "10.100.0.2:8080"]`),
  and to a wildcard (`["*"]`) — upstream's documented way to pre-allow a
  host with no prompt. All three still hit "needs your explicit approval
  ... network access to `10.100.0.2:8080`."
- `--dangerously-skip-permissions` in place of `--permission-mode
  acceptEdits` — upstream's documented answer for "fully unattended inside
  a container, VM, or the sandbox runtime." Made things *worse*: under it,
  `autoAllowBashIfSandboxed` stopped applying, and `git add`/`commit` —
  which worked fine under plain `acceptEdits` — started needing approval
  too. Reverted.

`dispatch.py` is currently back on `--permission-mode acceptEdits`, the
empirically-furthest-getting mode (gets through commit, blocks at push).

## Where to start

Two untried options, in the order worth trying:

1. **`--permission-mode dontAsk` + an explicit `permissions.allow` rule**
   for `Bash(git *)` (or narrower: `clone`/`fetch`/`checkout`/`add`/
   `commit`/`push`/`config`/`show`/`ls-remote`, matching exactly what
   `dispatch.py`'s prompt asks for). `docs/design.md` already named
   `dontAsk` as "the locked-down end" — never tried live. This sidesteps
   the ambiguous prompt/classifier path entirely: a matching `permissions.
   allow` rule means the tool call is approved outright, no prompt to
   answer.
2. **`--permission-prompt-tool`** — a small script that auto-answers
   permission prompts programmatically, upstream's own mechanism for
   dynamic headless approval. More flexible than a fixed allowlist, more
   code to write and test.

Either way, the fix belongs in `grain/automation/dispatch.py`'s
`_start_task` (the `claude -p ...` command string), the same place this
session's `acceptEdits`/`--dangerously-skip-permissions` experiment
lived. If a `permissions.allow` rule is the answer, it goes in
`provision/sandbox.sh`'s `settings.json` alongside the existing `sandbox`
block.

## How to reproduce/verify

This dev host already has everything needed — `/dev/kvm`, the cached base
image, and (check first) whether `~/.t/token` or an equivalent
`CLAUDE_CODE_OAUTH_TOKEN` is still available. The mock GitHub server this
session used was a scratch script, not checked in; rebuilding it is
mechanical (see `tests/test_live_issue_to_pr.py`'s `RealGitHubMock` and
`_GitBackendHandler` for the two pieces to combine — REST endpoints plus a
`git http-backend` CGI, one process, one port).

```sh
sudo python3 -m grain.cli --image /var/lib/grain/images/debian-12.qcow2 \
  host bootstrap --repo <owner>/<repo> --github-token-file - \
  --github-host <mock-host>:<port> --git-forward-host <mock-host>:<port> \
  --github-insecure-http <<< "mock-token"

# per sandbox, since --claude-credentials-file expects a
# ~/.claude/.credentials.json shape and a setup-token value isn't that:
ssh -i /var/lib/grain/admin-ssh debian@<sandbox-ip> \
  sudo systemctl set-environment CLAUDE_CODE_OAUTH_TOKEN=<token>

ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
  'cd /opt/grain && sudo python3 -m grain.cli automation run-once'
```

**One operational note from this session, worth remembering before running
this again:** this host runs the live pytest suites for real (it has
`/dev/kvm`), and those suites default to the same VM names
(`controller`, `sandbox-0`) a real deployment uses. Running the full test
suite while a real bootstrapped cluster is up will destroy it — happened
twice this session. Don't run `python3 -m pytest` (unscoped) while a live
cluster from `host bootstrap` is up; scope it to specific files, or stop
the cluster first.

**Also stop `grain-automation.timer` before walking away** if the push
issue isn't resolved — otherwise it retries the same failing dispatch
every two minutes indefinitely, burning real API calls on a known-blocked
push:

```sh
ssh -i /var/lib/grain/admin-ssh debian@10.100.0.2 \
  sudo systemctl stop grain-automation.timer
```
