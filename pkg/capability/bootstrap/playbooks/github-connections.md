# Bootstrap: the primary GitHub connection

Goal: give this deployment a working credential to push branches and
open pull requests against its real target repos -- what
`pkg/gitproxy.CredentialSet` reads from
`$GRAIN_DATA_DIR/secrets/github/`: a `credentials.json` mapping an
owner/repo (or owner/*, or *) pattern to a named credential, and one
`<name>.token` (or `<name>.app.json`) file per credential beside it.
Those files are the whole mechanism -- the SQLite secrets database
`grain secrets` writes holds the agent frameworks' own keys and is never
consulted for a GitHub credential, deliberately (see
`pkg/gitproxy/credentials.go`'s doc comment). This is different in kind from
the other three bootstrap playbooks: there is no Terraform involved, and
no broad "master" credential that mints a narrow one and then gets
dropped -- the credential you configure here *is* the one this
deployment keeps using. Don't apply this playbook's "drop it afterward"
instinct from the GCP playbooks to the PAT or App key below; that's the
one you're setting up to stay.

Every shell command below is something you (the agent) run through the
`run_host_command` tool, one at a time -- each one is posted into this
chat and waits for a human "approve" before it actually runs. As with
the GCP playbooks: **never** ask the human to paste a raw token or
private key into this chat and then embed it in a command -- anything
you pass to `run_host_command` is posted into this task's own
conversation for the approval step, and a real credential doesn't belong
sitting in a stored comment forever. Two ways to avoid that, in order of
preference:

1. Ask the human to paste it into the UI themselves: Settings -> GitHub
   -> Named tokens, a name and the token, "Save token". That writes the
   same `<name>.token` file everything below describes, with mode 0600,
   and the value never leaves their browser for this chat. Prefer this
   whenever they can reach the UI -- it is strictly less exposure than
   anything you can run, and it needs no shell access at all.
2. Otherwise, ask the human to place the credential in a file on the
   host themselves (out of band, the same way the GCP playbooks handle a
   master credential key file), then reference only its path in the
   command you run -- see step 2 below -- and delete that file
   afterwards.

## 1. Ask which shape of credential

Two supported shapes, per `terraform/gcp/deploy/push-secrets.sh`'s own doc
comment and `pkg/gitproxy`:

- **A fine-grained PAT**, scoped to the specific repos this deployment
  targets. Simpler, but a PAT cannot read the Checks API -- auto-merge
  degrades to "never actually merges" against one, reported by
  `AutoMergeDegraded` in the UI's own `/api/config` (see
  `pkg/ui.Config`'s doc comment).
- **A GitHub App installation** (App ID + installation ID + private
  key), which *can* read Checks. Registering the App itself is a
  separate, manual, browser-based step -- see github-test-repos.md,
  which walks through `grain controller bootstrap-github-app` for
  exactly that. If the human already has an App from that flow, reuse
  its ID and key here instead of registering a second one.

Ask which one they want, and -- for either -- which owner/repo pattern
it should cover (`*` for every repo this deployment ever targets,
`owner/*` for one organization, or an exact `owner/repo`).

## 2. Set the credential material

For a PAT, name it (e.g. `bot`) and store it as
`$GRAIN_DATA_DIR/secrets/github/bot.token`, whose name is the credential
name step 3's `credentials.json` will reference. Two ways, matching the
two options above:

- The human pastes it into Settings -> GitHub -> Named tokens as `bot`.
  That writes exactly that file; nothing further is needed here.
- Or, from a file they placed on the host out of band:

```
install -m0600 -o grain -g grain /tmp/grain-bootstrap-token.txt \
  "$GRAIN_DATA_DIR/secrets/github/bot.token" && rm -f /tmp/grain-bootstrap-token.txt
```

(`-o`/`-g` is whoever the daemon runs as -- `$GRAIN_USER` in
`scripts/setup.sh`, `grain` on a deployment set up by it. Check with
`ls -l "$GRAIN_DATA_DIR/secrets/github"` rather than assuming.)

For a GitHub App, three values in one JSON file,
`$GRAIN_DATA_DIR/secrets/github/<name>.app.json`, with the keys
`app_id`, `installation_id` and `private_key` -- the same trio
`push-secrets.sh` pushes as `GRAIN_GITHUB_APP_*` (all three together,
never a partial set). `private_key` is the PEM exactly as GitHub handed
it out, newlines and all, which makes this one to have the human write
themselves rather than build a JSON literal in a command you post here.
The UI's token field cannot write this shape, and says so.

## 3. Wire the credential name to a repo pattern

`credentials.json` under `$GRAIN_DATA_DIR/secrets/github/` maps a
pattern to the credential name from step 2 -- `gitproxy.core.go`'s own
error message points here when a push has no match:

```
{"owner/repo": "bot", "owner/*": "bot", "*": "bot"}
```

Ask the human for the exact pattern(s) this deployment needs (matching
`pkg/model.Config.TargetRepos`/this deployment's own default target),
edit the file with `run_host_command`, and restart the daemon for it
to pick up the change -- it reads this file once at startup, not
hot-reloaded. This one is host work either way: the Settings pane lists
the patterns pointing at each credential but does not edit them, since
which repos fall back to which credential is a deployment-wide decision
that a wrong click would take every push down with.

After the restart, Settings -> GitHub shows `bot` as the deployment
default rather than as a named token, which is the quickest confirmation
that the pattern file and the credential file agree.

## 4. (Optional) Extra named tokens, one per capability

Everything above configures the credential *every* repo falls back to.
A deployment can also hold extra named tokens -- a second machine
account, a token carrying a scope the default one deliberately withholds
(`workflow`, per docs/design.md's "Scopes to withhold") -- stored exactly
the way step 2 stored the default one, under a different credential name,
and needing no `credentials.json` entry at all. Settings -> GitHub ->
Named tokens is the ordinary way to add one, and the whole flow: name,
value, "Save token", restart.

Each of those names becomes a capability of its own,
`github-credential:<name>`, offered in the per-task capability picker and
listed on Settings' Capabilities tab. A task holding one pushes and pulls
through that token instead of the ladder above, for that task only; the
token itself never enters the sandbox. Like the ladder, the names are
read once at daemon startup, so a token added now needs a restart before
it can be ticked on anything.

Only offer this if the human asks for a second token or describes work
one task needs wider (or narrower) access for. One default credential is
the ordinary shape.

## 5. Verify

Ask the human to confirm a real push against one of the covered repos
succeeds (an ordinary task's own dispatch is the real test), or, if
`AutoMergeDegraded` matters to them, that Checks are readable -- only
true for the App shape, never the bare-PAT one.
