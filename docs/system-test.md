<!-- Written by AI on 2026.10.11 by model claude-opus-5.5; updated on 2026.10.29 by model GPT-6.1 Terra; rewritten on 2026.11.01 by model claude-opus-5.5. -->

# System test

Run this before a live deploy:

```sh
scripts/dev/system-test
```

It takes about 20 seconds and spends no tokens. Each group of outcomes runs on
its own isolated system, and the groups run at once. A system is a build of
this working tree under a private launchd job on a spare port, with its own
data directory and fake accounts, and fake providers in place of OpenAI and
Anthropic. Two groups start from the newest published release, so their first
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
| resets | Claude grants appear in status; the reset command checks pool availability and the next reset, chooses the earliest credit, and retries a response the published release lost with the same request ID, as after an update. |
| reset-rollback | A reset this build leaves pending when the provider drops its reply completes with the published release, as after a rollback. |
| routing | New Codex tasks and Pi sessions go to an account whose week has not started, then to the weekly allowance that resets first. They and their Codex forks stay on their account when another account becomes first, and move for a hard reserve. |
| routing | A request without a task ID is served. |
| exhaustion | When a task's account runs out, its next turn moves to another account. No model work is replayed, no tool effect repeats, and Codex shows no error. |
| outage-goal | With every account out of allowance, a running goal continues by itself when one account comes back. |
| claude | Claude Code gets a reply through the HTTPS proxy, recorded once with its tokens. |
| claude-throttle | When Anthropic throttles a Claude request without naming a usage limit, the account is held for the throttle's Retry-After and no longer, then serves the task. |
| claude-fast-mode | When Anthropic refuses fast mode for want of usage credits, Claude Code carries on at standard speed on the same account. A usage limit on a fast-mode request still moves the task. |
| claude-failover | When a Claude task's account reaches its usage limit, the task's next prompt is answered by another account and Claude Code shows no limit. |
| claude-limited | With every Claude account at its usage limit, Claude Code stops at once and says when the limit resets, and the next task gets the same answer from the proxy without a request to Anthropic. |
| retire | A retiring proxy finishes a reply it holds, closes an idle HTTP connection at once and a silent tunnel after its grace. A transferring tunnel keeps going until the switch limit, when the old process has gone. |
| outage | During a provider outage, launchd restarts a killed proxy, and a failed request is enough to start a waiting update. A candidate that cannot read the accounts stops, and its build is not skipped. |
| failed-release | Provider errors and client cancellations do not count against a candidate. Malformed upstream responses do: the candidate steps aside and status shows the skipped build. |
| queue | Two releases queued during a switch converge on the newest, with at most two proxy processes and no downgrade. |
| every group | Each completed response is recorded once, with its account, task and tokens. |

The command exits 0 only if every outcome held and nothing is left running.

## What is real

- The proxy: the build of this tree (or, for the first update, the newest
  published release or v0.1.9) under launchd, with the real installer,
  `deploy/install-proxy.sh`.
- The official Codex client: the one inside the desktop app
  (`/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex`), or
  `codex` on `PATH`. It runs `codex exec` tasks and `/goal` through
  `codex app-server`, and runs the scripted model's tool calls itself.
- Pi in sessions, if `pi` is on `PATH`.
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
number of requests, refused WebSockets, held or malformed responses, provider
and usage-read errors, a Claude throttle with a one-second Retry-After as
Anthropic sent on 31 October 2026, and a Claude five-hour limit. It refuses
every fast-mode Claude request as Anthropic does for an account without usage
credits. It logs every model request with a fingerprint of its bytes, so the
test can see that none was sent twice, and every usage read with the level it
reported, so the test can wait until the proxy has read a level.

It also serves HTTPS with a certificate authority made for each run. Codex
sends its own ChatGPT requests, such as its account check, only to an HTTPS
origin, and trusts only that authority, so none of its requests reach a real
service.

Timers are shortened; their live values are unchanged:

| Timer | Live | Test |
| --- | --- | --- |
| Installer proving period | 5 minutes | 1 second |
| Installer switch limit | 30 minutes | 6 seconds |
| Account reads (`CODEX_SWITCH_ACCOUNT_READ_SECONDS`) | 1 minute | 1 second |
| Idle Claude connection grace on retiring (`CODEX_SWITCH_CONNECTION_DRAIN_SECONDS`) | 1 minute | 1 second |
| launchd's wait before restarting a job that exited | 10 seconds | 1 second |

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

The next run moves this directory to the Trash.
