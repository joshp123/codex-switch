<!-- Written by AI on 2026.10.11 by model claude-opus-5.5; updated on 2026.10.29 by model GPT-6.1 Terra. -->

# System test

Run this before a live deploy:

```sh
scripts/dev/system-test
```

It takes about an hour. It keeps a tunnel transferring until the 30-minute
update limit, then runs further five-minute proofs. It starts from the newest
published release, updates it to builds of this working tree, runs the account
pool in isolation, and prints what the system did for each outcome:

1. An update waits for model work before it switches.
2. New Codex tasks and Pi sessions go to the account with the most weekly
   allowance left, and stay there when another account takes the lead.
3. Claude Code gets a reply through the HTTPS proxy, on the Claude account,
   recorded once with its tokens.
4. A request without a task ID is served.
5. When a task's account runs out, its next turn moves to another account. No
   model work is replayed, no tool effect repeats, and Codex shows no error.
6. With every account out of allowance, a running goal continues by itself
   when one account comes back.
7. An update adopts the published release's single job during work. Responses
   in flight over WebSocket and HTTP finish on the old process, while new work
   starts on the new one.
8. A retiring proxy closes an idle HTTP connection at once and a silent tunnel
   after a minute. A transferring tunnel keeps going until the 30-minute limit,
   when the old process has gone.
9. During a provider outage, launchd restarts a killed proxy and it serves. A
   candidate that cannot read the accounts stops, and its build is not skipped.
10. Provider errors and client cancellations do not count against a candidate.
    A local routing failure on an open WebSocket and malformed upstream
    responses do count: the candidate steps aside and status shows the skipped
    build.
11. Two releases queued during a switch converge on the newest, with at most
    two proxy processes and no downgrade.
12. Each completed response is recorded once, with its account, task and tokens.

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
a given number of requests, refused WebSockets, failed usage reads, and
responses held mid-stream for one client. It can also send malformed HTTP responses to test a failed
candidate. The test also temporarily renames a table in the isolated database
so the next request on an open WebSocket hits a local routing failure. The
proxy binaries built from this tree differ only in their build numbers.

The isolated Codex provider uses a 15-minute stream-idle timeout so a reply
deliberately held through the five-minute proof does not hit the client's
normal five-minute timeout first. It uses the same proxy URL, authentication,
Responses API and WebSocket support.

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
