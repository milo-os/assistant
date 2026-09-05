# milo-assistant — the assistant as a datumctl plugin

Makes Patch reachable as `datumctl assistant`, so the assistant is one verb inside
the CLI people already use for Datum Cloud rather than a separate binary with
its own login.

```
datumctl assistant                                                  # full-screen chat
datumctl assistant resume                                           # pick up a past conversation
datumctl assistant chat "Why is the api-backend workload not available?"   # one-shot, for pipes
datumctl assistant conversations list
```

The bare verb is the chat, the way `claude` or `codex` on their own are: the
full-screen UI is the primary experience, and `chat "<message>"` is the
scriptable form. `chat` with no message opens the same UI; `chat -i` keeps a
line-based session for terminals that cannot run it.

`resume` opens the full-screen chat straight into a conversation picker in
the style of `claude --resume`: type to search the project's conversations
(newest first, each shown by its opening message), ↑/↓ to browse, ctrl+t to
preview a transcript, enter to pick up where it left off. `resume <context-id>`
skips the picker. The same picker is `/resume` inside `chat --tui`.

The binary is named `milo-assistant`, not `datumctl-assistant`. datumctl recognises
both prefixes on `$PATH` and treats `milo-` as marking *"portable milo-os
platform plugins (host-agnostic, e.g. milo-ipam)"* — which is what this is: the
assistant is a milo-os service, and the plugin is host-agnostic. The verb it
registers is `assistant`; the prefix names the binary, not the command.

The verb is `assistant`, not `patch`, even though Patch is the product's name:
datumctl already carries kubectl's resource verbs (`get`, `apply`, `delete`,
`diff`, `edit`), a built-in `patch` is the obvious next one, and a built-in
shadows a plugin of the same name silently. `assistant` also sits naturally
beside the built-in `ai`.

It shares every line of client code with the standalone `patch` binary — both
are thin mains over [`internal/patchcli`](../../internal/patchcli) — and differs
only in where three things come from:

| | `patch` | `datumctl assistant` |
| --- | --- | --- |
| Project | `--project` | `--project`, defaulted from `DATUM_PROJECT` |
| Token | `--token` / `PATCH_TOKEN` | `plugin.Token()` → datumctl's credentials helper; `PATCH_TOKEN` overrides it for the dev playground |
| Endpoint | `--url` / `PATCH_URL` | `--url` / `PATCH_URL` (unchanged — see *Open decisions*) |

## Build and dev-install

```bash
task build:plugin                       # → ./bin/milo-assistant
task build:plugin INSTALL=1             # …and install it for datumctl
task build:plugin VERSION=v0.2.0        # stamp the version in the manifest
```

`INSTALL=1` copies the binary to `~/.datumctl/plugins/datumctl-assistant` — a
filename that deliberately **does not match the binary's own name**, because the
two lookups datumctl performs are not symmetric:

- In its **managed directory** it resolves the verb `assistant` by trying
  `assistant` then `datumctl-assistant`, and never `milo-assistant`. A generic
  `assistant` is re-hashed
  on every invocation and rejected unless `plugins.json` holds a matching
  SHA256, which only `datumctl plugin install` writes — so the legacy
  `datumctl-` filename is the one layout a local build can use unaided.
- On **`$PATH`** it tries `datumctl-assistant` then `milo-assistant`, so the shipped
  name works directly — but a PATH plugin is *blocked* until trusted, and trust
  records the SHA256, so **every rebuild invalidates it**.

The PATH route is the one that matches how this plugin ships, and for a rebuild
loop the env var is the only practical way to hold trust:

```bash
cp ./bin/milo-assistant /usr/local/bin/    # anywhere on $PATH
export DATUMCTL_TRUSTED_PLUGINS=assistant  # keyed on the verb, not the filename
```

Without one of those, `datumctl assistant` fails with *"unmanaged plugin that has
not been trusted"*, and — more confusingly — tab-completion silently returns
nothing.

## Contract

datumctl injects six variables and never passes a token among them:

| Variable | Used for |
| --- | --- |
| `DATUM_PROJECT` | default for `--project`, sent as `message.metadata.projectName` |
| `DATUM_ORG` | accepted by the SDK's `--org`; unused today |
| `DATUM_CREDENTIALS_HELPER` | absolute path to datumctl; execed for each token |
| `DATUM_SESSION` | passed to the helper as `--session` |
| `DATUM_API_HOST` | the Milo control plane — **not** the assistant (see below) |
| `DATUM_PLUGIN_API_VERSION` | `1`; the manifest declares the same |

Tokens are resolved **per request**, not once at startup, so a long `--tui`
session outlives the short-lived token it began with. Setting `PATCH_TOKEN`
bypasses the helper entirely — the way to point the plugin at the kind
playground, whose static dev token datumctl cannot mint (and whose aggregated
API is only reachable through its kubeconfig, hence `--kubeconfig`):

```bash
export PATCH_URL=http://localhost:1986 PATCH_TOKEN=pg-demo-token
datumctl assistant resume --project demo-project --kubeconfig .test-infra/kubeconfig
``` Each call is bounded by a
10s timeout because `plugin.Token()` takes no context and would otherwise hang
the plugin on a wedged helper.

`conversations`, `gaps` and endpoint discovery read the aggregated API with the
**same datumctl credentials**, against the project the caller selected. They
used to use `kubectl` and the ambient kubeconfig instead, on the reasoning that
reading a Kubernetes API deserves the caller's Kubernetes identity. Milo accepts
the token datumctl already mints, so that bought no extra identity — only a
second way to be pointed at the wrong server, since kubectl's current context
has nothing to do with the datumctl context. `--kubeconfig` still selects the
kubectl path for anyone who wants it. `resume` lists and loads conversations the same way.

Exit codes are the standalone CLI's — `0` completed, `1` request/stream failure
or a task that did not complete, `2` usage or configuration error — plus `130`
for an interrupted run, matching what datumctl reports for its own `^C`.

`-o` maps datumctl's output flag onto the two renderings this CLI has: `json`,
or `table`/`text` for the human one. `-o yaml` is rejected rather than silently
ignored.

## Open decisions

Four things are deliberately not settled here.

**Endpoint discovery.** Resolved. The service publishes its address as a
cluster-scoped `AssistantEndpoint` in the `assistant.miloapis.com` aggregated
API, and `serviceURL` reads it when neither `--url` nor `PATCH_URL` is set. That
was the third of the candidates below, chosen over a convention on
`DATUM_API_HOST` (which names the control plane, not the assistant) and a
datumctl config key (still per-machine setup): the CLI already reaches this API
with the caller's datumctl credentials for `conversations` and `gaps`, so
discovery needs no new credential and no hostname convention to keep compatible.

The resource reports the same `PUBLIC_BASE_URL` the service puts in its agent
card, so the two cannot disagree. An unset `PUBLIC_BASE_URL` is reported empty
rather than guessed, and the CLI turns that into an error naming the unset
setting instead of a bare "set PATCH_URL".

`--url`/`PATCH_URL` still win, so pointing at a local or preview instance needs
nothing unset.

Requires the aggregated apiserver (`config/base/assistant-apiserver`) to be
deployed. Where it is not — staging today, which serves `config/base` only —
discovery fails the same way `conversations` and `gaps` already do, and
`PATCH_URL` remains the fallback.

Related: the a2a-go client POSTs to the URL the **agent card advertises**, not
the one it fetched the card from. A misconfigured `PUBLIC_BASE_URL` will
therefore redirect a production client somewhere unintended.

**Entitlement gate.** compute gates its plugin on a `ServiceEntitlement` through
`go.miloapis.com/service-catalog/pkg/activation`, with a `datumctl compute
access` command to request one. Nothing equivalent is wired here. If Patch
should be entitlement-gated, that is a `PersistentPreRunE` on the root plus an
`access` subcommand. Note the package to target is the service-catalog one —
compute still pins `go.datum.net/datumctl/serviceactivation`, which never
landed on datumctl's main branch.

**Release pipeline.** None exists, here or anywhere at Datum: compute has a
correct `.goreleaser-plugin.yaml` that no CI job invokes, and its index entry is
hand-edited. Shipping this means writing the first one — a six-artifact
`os_arch` matrix, a `checksums.txt`, and a job that opens the PR against
`datum-cloud/datumctl-plugins`.

The `milo-` prefix splits the two install routes here, so pick one before
cutting a release. Via the **index**, the archive URI is whatever the manifest
says and extraction prefers a `milo-assistant` member over `datumctl-assistant`
over bare `assistant` — the shipped name works and the archive filename is free. Via
**`datumctl plugin install owner/repo`**, the asset name is not free: that path
hardcodes `"datumctl-" + name`, so it will only ever fetch
`datumctl-assistant_Darwin_arm64.tar.gz`. Supporting both means publishing under the
`datumctl-` asset name while keeping `milo-assistant` as the member inside.

**Overlap with `datumctl ai` and `datumctl console`.** datumctl already ships a
chat agent: a one-shot/REPL `ai` command and a full chat pane in `console`, both
running their own agent loop against Anthropic/OpenAI/Gemini with a user-held
API key. Patch is the opposite architecture — server-side, capabilities composed
from what a project is entitled to, metered. Two chat verbs with different auth
and different billing needs a product answer. There is no technical path to
merging them today: datumctl lists TUI panel extension points and MCP tool
registration as unbuilt V2 work, so a plugin can neither add a pane to `console`
nor register tools with `ai`.

## Not yet surfaced

The assistant emits tool-call events internally, but the A2A adapter drops
them — `RunSink` carries only `OnTextDelta` — so a long turn shows a spinner and
then prose, never which capability it invoked. Skill loading and token usage are
invisible for the same reason. Fixing it is a service-side change
(`cmd/assistant/runner.go`), not a plugin one, and it is the biggest single
improvement available to this UI.
