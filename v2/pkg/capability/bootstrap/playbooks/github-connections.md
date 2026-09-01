# Bootstrap: the primary GitHub connection

Goal: give this deployment a working credential to push branches and
open pull requests against its real target repos -- what
`v2/pkg/gitproxy.CredentialSet` reads from
`$GRAIN_DATA_DIR/secrets/github/credentials.json`, an owner/repo (or
owner/*, or *) pattern mapped to a named credential, resolved against
`$GRAIN_DATA_DIR/secrets`'s own store. This is different in kind from
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

1. Ask the human to place the credential in a file on the host
   themselves (out of band, the same way the GCP playbooks handle a
   master credential key file), then reference only its path:
   `grain secrets set -value-file /tmp/grain-bootstrap-token.txt github
   <credential-name>`, followed by deleting that file.
2. If `grain secrets set` is run with no `-value-file`, it reads the
   value from stdin -- ask the human to run that one command themselves,
   at their own terminal on the host, rather than relaying the value to
   you at all. Prefer this whenever the human already has shell access;
   it's strictly less exposure than option 1.

## 1. Ask which shape of credential

Two supported shapes, per `terraform/gcp-v2/push-secrets.sh`'s own doc
comment and `v2/pkg/gitproxy`:

- **A fine-grained PAT**, scoped to the specific repos this deployment
  targets. Simpler, but a PAT cannot read the Checks API -- auto-merge
  degrades to "never actually merges" against one, reported by
  `AutoMergeDegraded` in the UI's own `/api/config` (see
  `v2/pkg/ui.Config`'s doc comment).
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

For a PAT, name it (e.g. `bot`) and store it:

```
grain secrets set -value-file /tmp/grain-bootstrap-token.txt github bot
```

(or have the human run the stdin form themselves, per the preference
above). `grain secrets set`'s own usage takes `<secret> <key>` -- the
secret here is `github`, and the key is the credential's own name
(`bot`), matching what step 3's `credentials.json` will reference.

For a GitHub App, three values under the same credential name --
`app-id`, `installation-id`, `private-key` -- following
`push-secrets.sh`'s own `GRAIN_GITHUB_APP_*` trio (set all three
together, never a partial set).

## 3. Wire the credential name to a repo pattern

`credentials.json` under `$GRAIN_DATA_DIR/secrets/github/` maps a
pattern to the credential name from step 2 -- `gitproxy.core.go`'s own
error message points here when a push has no match:

```
{"owner/repo": "bot", "owner/*": "bot", "*": "bot"}
```

Ask the human for the exact pattern(s) this deployment needs (matching
`v2/pkg/model.Config.TargetRepos`/this deployment's own default target),
edit the file with `run_host_command`, and restart the git proxy for it
to pick up the change -- it reads this file once at startup, not
hot-reloaded.

## 4. Verify

Ask the human to confirm a real push against one of the covered repos
succeeds (an ordinary task's own dispatch is the real test), or, if
`AutoMergeDegraded` matters to them, that Checks are readable -- only
true for the App shape, never the bare-PAT one.
