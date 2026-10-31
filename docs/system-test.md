<!-- Written by AI on 2026.10.11 by model claude-opus-5.5; updated on 2026.10.29 by model GPT-6.1 Terra and on 2026.10.31 by model claude-opus-5.5. -->

# System test

Run this before a live deploy:

```sh
scripts/dev/system-test
```

It takes about six minutes. It starts from the newest published release,
updates it to builds of this working tree with the real installer, runs the
account pool in isolation, and prints what the system did for each outcome,
with how long it took. Timers are shortened; nothing else changes:

| Timer | Live | Test |
| --- | --- | --- |
| Installer proving period | 5 minutes | 10 seconds |
| Installer switch limit | 30 minutes | 45 seconds |
| Account reads (`CODEX_SWITCH_ACCOUNT_READ_SECONDS`) | 1 minute | 15 seconds |
| Idle Claude connection grace on retiring (`CODEX_SWITCH_CONNECTION_DRAIN_SECONDS`) | 1 minute | 10 seconds |

1. An update waits for model work before it switches.
2. An update adopts the published release's single job during work. Responses
   in flight over WebSocket and HTTP finish on the old process, while new work
   starts on the new one and proves it. Every later outcome runs on builds of
   this tree.
3. New Codex tasks and Pi sessions go to the account with the most weekly
   allowance left, and stay there when another account takes the lead.
4. A request without a task ID is served.
5. When a task's account runs out, its next turn moves to another account. No
   model work is replayed, no tool effect repeats, and Codex shows no error.
6. With every account out of allowance, a running goal continues by itself
   when one account comes back.
7. Claude Code gets a reply through the HTTPS proxy, on a Claude account,
   recorded once with its tokens.
8. When Anthropic throttles a Claude request without naming a usage limit,
   the account serves the task again once the throttle ends.
9. When a Claude task's account reaches its usage limit, the task's next
   prompt is answered by another account and Claude Code shows no limit. This
   needs two Claude accounts with allowance left.
10. With every Claude account at its usage limit, Claude Code stops at once and
    says when the limit resets, and the next task gets the same answer from
    the proxy without a request to Anthropic.
11. A retiring proxy finishes a reply it holds, closes an idle HTTP connection
    at once and a silent tunnel after its grace. A transferring tunnel keeps
    going until the switch limit, when the old process has gone.
12. During a provider outage, launchd restarts a killed proxy, and a failed
    request is enough to start a waiting update. A candidate that cannot read
    the accounts stops, and its build is not skipped.
13. Provider errors and client cancellations do not count against a candidate.
    Malformed upstream responses do: the candidate steps aside and status
    shows the skipped build.
14. Two releases queued during a switch converge on the newest, with at most
    two proxy processes and no downgrade.
15. Each completed response is recorded once, with its account, task and tokens.

The command exits 0 only if every outcome held and nothing is left running.

## What is real

- The newest published release's proxy, then the proxy built from this tree,
  under their own launchd jobs, on a spare loopback port, with their own data
  directory.
- The official Codex client: the one inside the desktop app
  (`/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex`), or
  `codex` on `PATH` without the app. It runs `codex exec` tasks and `/goal`
  through `codex app-server`.
- Pi in sessions, if `pi` is on `PATH`. Without it the test says so at the
  start and runs everything else.
- OpenAI, with your signed-in accounts, on `gpt-6-luna` at low effort. One run
  uses about 30 model requests and 0.5 million input tokens, mostly cached.
- The newest Claude Code inside Claude Desktop, or `claude` on `PATH`, with
  every signed-in Claude account. It runs short prompts on the latest Haiku at
  low effort, using `HTTPS_PROXY` and
  `NODE_EXTRA_CA_CERTS` from `print-claude-config`. The proxy reaches
  `api.anthropic.com` through the gateway, which connects over TLS.
- The update path: `deploy/install-proxy.sh` against private launchd jobs.

## What is simulated

`scripts/dev/system-test-gateway` stands between the proxy and OpenAI and
Anthropic. It
forwards real traffic and logs a fingerprint of every model request it sends,
so the test can see that none was sent twice. Its control file simulates what a
test cannot wait for: a weekly allowance level, an account that runs out after
a given number of requests, refused WebSockets, failed usage reads, and
responses held mid-stream for one client, and for Claude a 429 that names
no usage limit, as Anthropic sent on 31 October 2026 (with a five-second
Retry-After), or a five-hour limit on one Claude account. It can also send malformed HTTP
responses to test a failed candidate. The proxy binaries built from this tree
differ only in their build numbers.

## Safety

The test reads the account logins from the live data directory and copies only
the access tokens. Refresh tokens stay behind, so the test cannot rotate a live
login. Each account needs an access token valid for at least another 20 minutes.
The test copies every signed-in Claude account the same way. It prints `SKIP Claude Code` if
none is signed in, none has 20 minutes left on its token, or Claude Code is absent;
the Claude outcome then fails.

It never touches the live proxy, its launchd job or data directory,
`~/.codex/config.toml`, Pi's or Claude Code's configuration or the menu app. The clients run
with their own homes inside the run directory. When the run ends, or fails, the
test removes its launchd jobs (a label derived from the checkout path, and its
`.a` and `.b` slots) and stops every process it started, then checks that none
remain.

## When an outcome fails

The failure line says what happened instead. The evidence of the last run is
in `.project-private/system-test/`:

| File | Contains |
| --- | --- |
| `data/proxy.log` | Which account each task got and why, moves, waits and drains |
| `data/codex-switch.db` | Task pins and recorded responses |
| `logs/gateway.log` | Every model request sent to OpenAI, refusal and held response |
| `logs/<task>.out`, `logs/<goal>.jsonl` | What Codex, Pi, Claude Code and the update command printed |
| `home/sessions/` | Codex's own record of each response it received |
| `control` | The simulation as it ended |

The next run moves this directory to the Trash.

## Claude accounts

[Claude setup](../README.md#claude-subscriptions) uses the same proxy, data
folder and account commands. Sign in with `login --claude`; print Claude Code's
persistent settings with `print-claude-config`, or Pi's provider entry with
`print-claude-config --pi`.
