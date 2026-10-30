<!-- Written by AI on 2026.10.11 by model claude-opus-5.5; updated on 2026.10.29 by model GPT-6.1 Sol, derived from the owner's steering. It is a map, not the authority: where it disagrees with the owner's own words or with the running system, they win. -->

# Codex Switch: a map for agents

## What the owner wants (derived)

One person pays for several ChatGPT subscriptions and wants Codex to use them as
one pool. In the owner's words: "i point codex dot app at this thing; i log in
twice, then i never think about it again and both accounts drain equally", and
"the dumber and simpler the better".

The owner's own list of what the product must do (4 September):

1. "i can use codex and load balance across my 3 accounts"
2. "i never get interrupted"
3. "as accounts become limited/unlimited its all seamless and 'just works'"
4. "we have a good UI that makes it obvious what's being used and where and
   ideally in which thread, using chatGPT first part APIs where appropriate"
5. "we have bluegreen deployments so that agents can develop on this without
   interrupting my work"

Since then: Pi uses the same pool; the menu should look like OpenUsage but nicer,
with accurate numbers, and show that the proxy is working; an agent with no
context must be able to run the product, read its output and logs, and find and
fix a problem; the repository will be published, so every commit must be
publishable. Later, not now: Claude support and server-side use.

## Hard rule: no hedging in the product

The owner, 1 August: if an agent is unsure how something works and adds a flag,
an information bar, a caveat, a "maybe", a range, a minimum or maximum, or any
other hedge to the UI, "you know it's completely fucked and the product's got
off track". The same goes for logs, status output and docs. A hedge means the
data or the mechanism is not understood yet: find the source of truth and fix
it, or remove the feature.

## Where the truth lives

| Question | Source |
| --- | --- |
| What did the owner actually ask for? | The owner's messages, if present locally: `.project-private/steering/README.md`. Later messages win. |
| How does the owner want engineering done? | `~/.codex/AGENTS.md` (the owner's own principles). |
| What does the accepted menu look like? | `.project-private/m3-mockups-r2/` (the approved design), `.project-private/visual-steering/accepted-direction/` (the original direction), then OpenUsage (`robinebers/openusage`). |
| Is the live proxy working, and how are accounts used? | `codex-switch status`; the log at `~/Library/Application Support/codex-switch/proxy.log`. |
| Which build is live? | `codex-switch status` names the serving build and skipped builds. `GET http://127.0.0.1:<port>/client/<capability>/codex-switch/deploy-status` reports build, PID, own failures and last completed model reply; the capability is in `client-capability` in the data directory. |
| What runs it? | `launchctl list \| grep codex-switch`; the launchd plist names the binary, port and data directory. |
| How do clients reach it? | Codex: `openai_base_url` in `~/.codex/config.toml` (`codex-switch print-codex-config`). Pi: `~/.pi/agent/models.json`, provider `openai-codex`. |
| How does Codex itself behave? | The Codex source (`openai/codex`), and the installed client: `/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex`. |
| Where does the menu get its numbers? | The serving proxy composes a snapshot once a minute and writes its private URL to `menu-snapshot-url` in the data directory; the menu and `codex-switch status` fetch it from there. |
| Does the whole system work before a deploy? | `scripts/dev/system-test`; see `docs/system-test.md`. |
| How is it installed, updated and removed? | `deploy/install.sh` builds the app and proxy; the app runs `deploy/install-proxy.sh <binary>` on launch. The installer serializes updates, selects the newest build, proves it in the second launchd slot and drains the old slot. `deploy/deploy.sh back` restores the previous binary. `deploy/uninstall.sh` removes both slots. `deploy/installation.sh` names the installation paths. Running these against the live install needs the owner's approval. |

## The code

| Path | Owns |
| --- | --- |
| `cmd/codex-switch/` | CLI commands, wiring, and composing and serving the menu snapshot. Logic does not belong here. |
| `internal/accounts/` | Sign-in, credentials, the account pool, and every read from OpenAI about an account. |
| `internal/routing/` | Which account serves a task, and when a task moves. |
| `internal/relay/` | Forwarding HTTP, SSE and WebSocket traffic byte for byte, recording completed responses, draining on update. |
| `internal/usage/` | Tokens and API-equivalent cost per account and per task, for Today, 7 Days and 30 Days. |
| `internal/store/` | The one SQLite database. Imports nothing of ours. |
| `internal/codexstate/` | Reading task titles and model settings from the installed Codex. |
| `internal/clientcapability/` | The private URL path that stops web pages from using the proxy. |
| `internal/taskidentity/` | The task ID a Pi session is routed and counted under. |
| `proto/`, `internal/menupb/` | The typed snapshot the menu renders. Regenerate the Go and Swift code with `scripts/dev/generate-menu-protobuf`. |
| `macos/CodexSwitchMenu/` | The menu bar app. |
| `deploy/`, `scripts/dev/` | Install, blue-green update and uninstall; build, test, stage and system-test scripts (`devenv tasks run dev:build`, `dev:test`, `dev:stage`, `scripts/dev/system-test`). |

## How the owner wants it proved

Declarative, repeatable, full tests in code. Test only the real path, not
invented mocks. Test as close to the real thing as possible: the real proxy
process, the real Codex and Pi clients, real accounts on a cheap model, a real
blue-green update, and the real menu app on screen. A test that only checks the
code against itself proves nothing.

## Live system

Never interrupt running work. Deploy the proxy only through
`deploy/install-proxy.sh`; deploy the menu separately. Do not commit email addresses, task IDs, local
paths or agent plans.

Clean up what you start. Run a test build of the menu as an app bundle, never
by opening its bare binary: macOS runs a bare binary in a Terminal window, and
Terminal starts it again at every login. Quit it when the test ends. Once a
branch is merged, move its worktree folder to the Trash, run `git worktree
prune`, and delete the branch.
