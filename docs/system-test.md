<!-- Written by AI on 2026.10.11 by model claude-opus-5.5; updated on 2026.10.29 by model GPT-6.1 Terra; rewritten on 2026.11.01 by model claude-opus-5.5; updated on 2026.11.08 and 2026.11.09 by model claude-opus-5.5; updated on 2026.11.09 by model claude-fable-5.2. -->

# System test

Run this before a live deploy:

```sh
scripts/dev/system-test
```

It takes about 20 seconds and spends no tokens. Each group of outcomes runs on
its own isolated system, and the groups run at once. A system is a build of
this working tree under a private launchd job on a spare port, with its own
data directory and fake accounts, and fake providers in place of OpenAI and
Anthropic. Three groups start from the newest published release, so their first
update is the one Macs really make, and one from v0.1.9, the last release that
names accounts only by their folders. The official Codex, Pi and Claude Code
clients drive each system; a scripted model answers them. The test prints what
each system did for every outcome, with how long it took, and leaves nothing
running.

`scripts/dev/system-test --only GROUP,GROUP` runs those groups.

Before a release, also run the smoke test (see below).

## Outcomes

| Group | Outcome |
| --- | --- |
| update | An update waits for model work before it switches. |
| update | An update adopts the published release's single job during work. Responses in flight over WebSocket and HTTP finish on the old process, while new work starts on the new one and proves it. A goal's tool call spans the update and runs once. |
| update | Records the published release wrote, including those it writes while it drains, keep their contents and their accounts' keys, and a task it pinned keeps its account. |
| keyless-update | Records v0.1.9, the last release before account keys, wrote by folder name, including those it writes while it drains, keep their contents and take their account keys, and a task it pinned keeps its account. |
| rollback | After an update, `deploy/deploy.sh back` restores the published release. Tasks pinned by either build stay on their accounts while new work goes to the account the rule picks. T3 reads every account's usage by the index the other build gave it, after the update and after the rollback. |
| rollback | A task the rolled-back published release moves off an exhausted account stays where it moved after the next update, although the first account has recovered. |
| resets | Status lists each Codex account's reset with its expiry; the reset command checks pool availability and the next reset, and retries a response the published release lost with the same request ID, as after an update. |
| auto-reset | Status lists each Codex account's reset, and automatic resets off until `codex-switch auto-reset codex on`. With every Codex account out and work waiting on two proxies, as on two Macs, only the one with automatic resets on spends a credit. It spends none while an account refills within 30 minutes, then one, on the account with the most of its week left. While OpenAI's usage reading still shows that account at its limit, neither it nor the reset command spends another, and the work on both resumes once the reading catches up. |
| cold-reset | A second proxy with automatic resets off, started after the first spent a credit while the reading still lags, spends none while its work waits. |
| authority-off | With automatic resets off and every Codex account out, no credit is spent; the work waits and resumes when an account refills. |
| expiry-reset | Of two proxies, only the one with automatic resets on spends a credit in its last hour. It keeps the credit while its account has used no more of its week than the share of the week that has passed, or when OpenAI finds nothing to reset; a later attempt, under a new request ID, spends it once, and the account's second credit is then kept. |
| authority-rollback | The proxy with automatic resets on spends a credit and loses the reply. `deploy/deploy.sh back` restores the published release, which spends nothing by itself, and its reset command finishes the pending reset. After the next update the proxy spends no second credit while the reading lags, and the work resumes on the reset account once it catches up. |
| secrets | When OpenAI's reason for refusing a model request, or another request, or for failing a response, quotes the access token across the 300 bytes of a reason the log keeps or the 4096 bytes the proxy reads, the proxy log holds no part of the token. |
| secrets | When OpenAI answers a request to spend a reset credit with the access token as its result, neither the automatic reset nor the reset command logs or prints any part of it. |
| secrets | While the account a credit reset is signed out, so OpenAI's usage reading cannot show the reset, no credit is spent, and the log says once that spending is paused and why. Work resumes on that account once it signs in again. |
| secrets | When Anthropic fails to refresh a Claude login with a message that quotes the refresh token, status shows why without any part of a credential. |
| routing | New Codex tasks and Pi sessions go to an account whose week has not started, then to the weekly allowance that resets first. They and their Codex forks stay on their account when another account becomes first, and move for a hard reserve. |
| routing | A request without a task ID is served. |
| exhaustion | When a task's account runs out, its next turn moves to another account. No model work is replayed, no tool effect repeats, and Codex shows no error. |
| websocket-age-limit | When OpenAI ends a goal's WebSocket at its one-hour limit during a tool call, the goal carries on over a new connection. No model work is replayed and Codex shows no error. |
| outage-goal | With every account out of allowance, a running goal continues by itself when one account comes back. |
| claude | Claude Code gets a reply through the HTTPS proxy, recorded once with its tokens. |
| claude | Status lists each Claude account's three Fable resets with their expiry, and not an expired one. Anthropic lists them only to the Claude Code command line, new enough, and the proxy reads as one. A login saved as Max 20x shows as Ultra. |
| claude-newer-client | When Anthropic lists stored resets only to a newer Claude Code than the proxy has served, the proxy log gives Anthropic's reason and status lists none. After one request from that Claude Code, the proxy's next usage read is as it and status lists the resets. |
| claude-fable-limit | With every Claude account's Fable allowance used up, Claude Code stops and says it has reached its Fable limit, not its weekly limit, and on another model it still gets a reply. |
| claude-throttle | When Anthropic throttles a Claude request without naming a usage limit, the account is held for the throttle's Retry-After and no longer, then serves the task. |
| claude-fast-mode | When Anthropic refuses fast mode for want of usage credits, Claude Code carries on at standard speed on the same account. A usage limit on a fast-mode request still moves the task. |
| claude-failover | When a Claude task's account reaches its usage limit, the task's next prompt is answered by another account and Claude Code shows no limit. |
| claude-limited | With every Claude account at its usage limit, Claude Code stops at once and says when the limit resets, and the next task gets the same answer from the proxy without a request to Anthropic. |
| claude-sign-out | When Anthropic refuses to refresh an expired Claude login with a message that quotes the refresh token, the proxy's sign-out line gives Anthropic's HTTP status, error code and message, and the proxy log holds no token. |
| retire | After a candidate slow to start, a retiring proxy still finishes a reply it holds, closes an idle HTTP connection at once and a silent tunnel after its grace. A transferring tunnel keeps going until the switch limit, when the old process has gone. |
| outage | During a provider outage, launchd restarts a killed proxy, and a failed request is enough to start a waiting update. A candidate that cannot read the accounts stops, and its build is not skipped. |
| failed-release | Provider errors and client cancellations do not count against a candidate. Malformed upstream responses do: the candidate steps aside and status shows the skipped build. |
| queue | Two releases queued during a switch converge on the newest, with at most two proxy processes and no downgrade. |
| every group | Each completed response is recorded once, with its account, task and tokens. |
| every group | No proxy log or launchd output holds any part of a credential in the run's logins: 16 characters of one in a row. Lines the published releases wrote, which predate this check, are left out. |

The command exits 0 only if every outcome held and nothing is left running.

## What is real

- The proxy: the build of this tree (or, for the first update, the newest
  published release or v0.1.9) under launchd, with the real installer,
  `deploy/install-proxy.sh`.
- The official Codex client: the one inside the desktop app
  (`/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex`), or
  `codex` on `PATH`. It runs `codex exec` tasks and `/goal` through
  `codex app-server`, and runs the scripted model's tool calls itself. A goal
  is set as the Codex app sets one, and Codex starts its first turn.
- Pi in sessions, if `pi` is on `PATH`.
- A second proxy, built from this tree, for the groups that test two Macs. It
  has its own data directory, Codex home and launchd job, and is signed in to
  the same accounts in the opposite order, so its folders name them differently.
- The newest Claude Code inside Claude Desktop, or `claude` on `PATH`, through
  `HTTPS_PROXY` and `NODE_EXTRA_CA_CERTS` from `print-claude-config`.

## What is fake

`scripts/dev/fake-providers` stands in for OpenAI and Anthropic. It answers
every endpoint the proxy and the clients use, and keeps each conversation as
the Responses API does. Its model follows scripts written into the prompts:
`RUN: <command>` asks the client to run a command, `GOAL COMPLETE` marks the
goal complete and `REPLY: <text>` ends the turn. A prompt asking for a word gets
that word. The accounts are fake logins only it accepts.

Its control file simulates what a test cannot otherwise arrange: weekly
allowance levels, exhausted accounts, an account that runs out after a given
number of requests, refused WebSockets, a WebSocket ended at OpenAI's one-hour limit, held or malformed responses, provider
and usage-read errors, a Claude throttle with a one-second Retry-After as
Anthropic sent on 31 October 2026, and a Claude five-hour limit. It refuses
every fast-mode Claude request as Anthropic does for an account without usage
credits. It refuses every Claude login refresh with Anthropic's error, or
with HTTP 503 when the test asks. Every error it returns quotes the
credentials of the request it answers, as a careless provider might: the
Authorization header, or the refresh token sent. Its logins have random
tokens, so a test can find any part of one in what the proxy writes. It holds reset credits and spends them
as OpenAI does: once per request ID, and `no_credit` for a credit spent by
another request, expired or never held. A request ID names one attempt, as
Codex's own client treats it, so a repeat gets the attempt's first answer. It answers `nothing_to_reset` for an
account under 20% used; OpenAI documents no such level, so the test picks one
to make that answer happen. It can hold requests to spend a credit, lose
every reply to one, and make its usage reading lag behind a spend. It can
refuse requests, or fail a response, with a reason that quotes their access
token where the proxy shortens what it logs, and answer a request to spend a
credit with the access token as its result. It logs every model request with a fingerprint of its bytes, so the
test can see that none was sent twice, and every usage read with the level it
reported, so the test can wait until the proxy has read a level.

It also serves HTTPS with a certificate authority made for each run. Codex
sends its own ChatGPT requests, such as its account check, only to an HTTPS
origin, and trusts only that authority.

Codex also reads the machine's own configuration, `/etc/codex/config.toml`, and
a release build cannot skip it. The test's Codex configuration turns off
plugins, whose sync fetches from GitHub, and every MCP server that file starts.
The file's other settings, such as features and skills, still apply. The
clients run without the proxy variables of the shell that started the test,
such as `HTTPS_PROXY`, so no client request goes through a proxy outside the
test. With these, the test's Codex opens no connection outside the machine.

Timers are shortened; their live values are unchanged:

| Timer | Live | Test |
| --- | --- | --- |
| Installer proving period | 5 minutes | 1 second |
| Installer switch limit | 30 minutes | 15 seconds |
| Account reads (`CODEX_SWITCH_ACCOUNT_READ_SECONDS`) | 1 minute | 1 second |
| Idle Claude connection grace on retiring (`CODEX_SWITCH_CONNECTION_DRAIN_SECONDS`) | 1 minute | 1 second |
| Menu snapshot (`CODEX_SWITCH_MENU_SNAPSHOT_SECONDS`) | 1 minute | 1 second |
| launchd's wait before restarting a job that exited | 10 seconds | 1 second |

The switch limit counts the candidate's start and proof. In the test, as live, it
leaves the old proxy time to finish its replies after a slow start.

The test does not wait out a timer to see that something happened: it waits for
the event, such as the fake providers answering a usage read, or a file the test
creates to release a tool call.

## Smoke test

```sh
scripts/dev/smoke-test
```

It checks that the real services still accept the proxy's requests. It starts
one proxy built from this tree under a private launchd job, with copies of the
live logins' access tokens, and sends one prompt each from Codex, Pi and Claude
Code on the cheapest models. That costs three small requests and takes about 20
seconds. Refresh tokens stay behind, so it cannot rotate a live login; each
copied token needs ten minutes left. It never touches the live proxy, its
launchd job or data directory, or any client's configuration.

## When an outcome fails

The failure line says what happened instead. Each group's evidence is in
`.project-private/system-test/<group>/`:

| File | Contains |
| --- | --- |
| `data/proxy.log` | Which account each task got and why, moves, waits and drains |
| `data/codex-switch.db` | Task pins and recorded responses |
| `logs/fake-providers.log` | Every model request the fake providers received, each refusal, held response and usage read |
| `logs/requests/` | A copy of every OpenAI model request, by fingerprint |
| `logs/<task>.out`, `logs/<goal>.jsonl` | What Codex, Pi, Claude Code and the installer printed |
| `home/sessions/` | Codex's own record of each response it received |
| `system-test.out` | The group's own output |

The next run deletes this directory.
