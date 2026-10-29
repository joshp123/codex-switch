<!-- Written by AI on 2026.10.11 by model claude-opus-5.5; updated on 2026.10.29 by model GPT-6.1 Terra. -->

# System test

Run this before a live deploy:

```sh
scripts/dev/system-test
```

It takes at least 40 minutes: it keeps a tunnel transferring until just before the 30-minute update limit and runs further five-minute proofs. It builds the proxy from this working tree, runs
the account pool in isolation, and prints what the system did for each outcome:

1. New Codex tasks and Pi sessions go to the account with the most weekly
   allowance left, and stay there when another account takes the lead.
2. Claude Code gets a reply through the HTTPS proxy, on the Claude account,
   recorded once with its tokens.
3. A request without a task ID is served.
4. When a task's account runs out, its next turn moves to another account. No
   model work is replayed, no tool effect repeats, and Codex shows no error.
5. With every account out of allowance, a running goal continues by itself
   when one account comes back.
6. A blue-green update adopts the old single-job installation during work.
   Responses in flight over WebSocket and HTTP finish on the old process,
   while new work starts on the new one. An idle Claude tunnel closes and a
   transferring tunnel stays open until just before the 30-minute limit.
7. A candidate that receives malformed upstream responses steps aside, the
   previous proxy resumes, and status shows the skipped build.
8. Two releases queued during a switch converge on the newest, with at most
   two proxy processes and no downgrade.
9. Each completed response is recorded once, with its account, task and tokens.

The command exits 0 only if every outcome held and nothing is left running.

## What is real

- The proxy binary built from this tree, under its own launchd job, on a spare
  loopback port, with its own data directory.
- The official Codex client: the one inside the desktop app
  (`/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex`), or
  `codex` on `PATH` without the app. It runs `codex exec` tasks and `/goal`
  through `codex app-server`.
- Pi in sessions, if `pi` is on `PATH`. Without it the test says so at the
  start and runs everything else.
- OpenAI, with your signed-in accounts, on `gpt-6-luna` at low effort. One run
  uses about 30 model requests and 0.5 million input tokens, mostly cached.
- The newest Claude Code inside Claude Desktop, or `claude` on `PATH`. It runs
  one short prompt on the latest Haiku at low effort, using `HTTPS_PROXY` and
  `NODE_EXTRA_CA_CERTS` from `print-claude-config`. The proxy connects directly
  to `api.anthropic.com` over TLS; Claude traffic never goes through the gateway.
- The update path: `deploy/install-proxy.sh` against private launchd jobs.

## What is simulated

`scripts/dev/system-test-gateway` stands between the proxy and OpenAI. It
forwards real traffic and logs a fingerprint of every model request it sends,
so the test can see that none was sent twice. Its control file simulates what a
test cannot wait for: a weekly allowance level, an account that runs out after
a given number of requests, refused WebSockets, and responses held mid-stream
for one client. It can also send malformed HTTP responses to test a failed
candidate. The proxy binaries use the same source and distinct build numbers.

## Safety

The test reads the account logins from the live data directory and copies only
the access tokens. Refresh tokens stay behind, so the test cannot rotate a live
login. Each account needs an access token valid for at least another hour.
The test copies one signed-in Claude account. It prints `SKIP Claude Code` if
none is signed in, none has an hour left on its token, or Claude Code is absent;
the Claude outcome then fails.

It never touches the live proxy, its launchd job or data directory,
`~/.codex/config.toml`, Pi's or Claude Code's configuration or the menu app. The clients run
with their own homes inside the run directory. When the run ends, or fails, the
test removes its launchd jobs (`dev.codex-switch.system-test-terra` and its
`.a` and `.b` slots) and stops every process it started, then checks that none remain.

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
