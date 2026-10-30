# Codex Switch

Use several ChatGPT or Claude accounts as one pool, without ever logging out.

<img src="docs/images/menu-demo.png" alt="The Codex Switch menu cycling through Today, 7 Days and 30 Days, in API-equivalent cost and then in tokens, with account emails hidden" width="344">

## Why

When GPT-5.6 Sol came out, one or two Codex threads could use up a five-hour
limit in one to two hours. I had two accounts and balanced work between them by
hand. It wasn't enough.

The ChatGPT app is built for one account at a time. To use a second account you
log out and log in again, and that breaks things: Codex Remote stops working,
memory stops working, and running tasks lose their place. There is no way to
pool usage across accounts.

Codex Switch is a transparent proxy. You stay logged in to your main ChatGPT
account everywhere, and Codex Switch spreads the work across all your accounts
behind the scenes. The Claude app, Claude Code and Pi can pool Claude subscriptions too.

Ironically, Claude Opus 5.5 is what finally let me ship it.

## How it works

Codex lets you set a different API address. Companies use the same setting to
put their own gateway in front of OpenAI. Codex Switch sits at that address as a
drop-in, so the ChatGPT app and the Codex CLI just work. Nothing is patched.

- It all runs locally on your Mac. Need it on two machines? Install it on both.
- You sign each account in once, with Codex's normal ChatGPT login. Codex Switch
  keeps every account's tokens refreshed after that.
- It does not use OpenAI's "Sign in with ChatGPT" for apps yet. That may come
  later.

## Is this allowed?

Use it at your own risk. You're probably not supposed to do this: it relies on
ChatGPT endpoints that OpenAI doesn't document, and pooling accounts may go
against their terms. Until OpenAI supports several accounts in the app natively,
it's the only way I've found to make this work. Tibo, if you're reading this:
please add it, fix the limits, and ship a model better than Opus 5.5. lol

It is macOS only, and so far it has one user: me.

## What it does

- Codex (the app, the CLI and sub-agents) and [Pi](https://github.com/badlogic/pi-mono)
  all point at one local URL.
- The Claude app's Code tab, Claude Code and Pi use the same pool for Claude subscriptions.
- A new task goes to whichever account has the most weekly allowance left.
- A task stays on its account, so its prompt cache stays warm.
- When an account runs out, the task moves to another account on its next turn.
  Nothing is replayed and the agent just carries on.
- Updating the proxy never drops work in progress.
- The menu bar shows what's left on each account, what each account and task
  used, and that the pool is working.

## The menu

The menu bar shows the weekly allowance left on each account.

<img src="docs/images/menu-bar-item.png" alt="Menu bar item: account 1 at 96% and account 2 at 96%" width="140">

Click it for:

- whether the pool is working, and how many accounts are in it
- cost at API prices, or tokens, for Today, 7 Days or 30 Days, split by account
- each account's allowance, when it resets, and its usage over the last 30 days
- when an account will run out, if it's going to run out before it resets
- the tasks that used the most, and which accounts served them

The eye button hides emails so you can post screenshots. Account 3 below is a
free account that can't run the model I use, so the work goes to the other two.

<img src="docs/images/menu-panel.png" alt="The full menu for Today in API-equivalent cost, with emails hidden and task names blurred" width="344">

ChatGPT account totals come from OpenAI's own daily usage report, so they
include work done on other machines. Claude totals come from completed replies
through the proxy. Task names come from the clients' session logs on your Mac.

## Install

Paste this into Codex or Claude Code:

```text
Install Codex Switch from https://github.com/joshp123/codex-switch on this Mac.
Read its README first. Download CodexSwitchMenu.zip from the latest GitHub
Release, unzip it, move CodexSwitchMenu.app to Applications and open it. Then
help me sign in each of my ChatGPT accounts with `codex-switch login` and Claude
accounts with `codex-switch login --claude` (I'll do the browser part). Configure only the
clients I use: `print-codex-config` for Codex and Pi's openai-codex provider;
`print-claude-config` for Claude Code's settings.json env;
`print-claude-config --pi` for Pi's anthropic provider. Merge the printed
settings into the existing files without replacing other settings. Finish with
`codex-switch status` and show me that every account is available.
```

The only thing you do yourself is sign in to each account in the browser.
The app installs the proxy and starts it at login. No build tools are needed.

### Doing it by hand

You need macOS 14 or later, your coding client, and at least two subscriptions
for the provider you use.

**1. Install.**

Download [CodexSwitchMenu.zip from the latest release](https://github.com/joshp123/codex-switch/releases/latest),
unzip it, move CodexSwitchMenu.app to Applications and open it.
It runs the proxy under launchd on
`127.0.0.1:2455`, so it starts at login and restarts if it stops. The
`codex-switch` command goes in `~/.local/bin`, so make sure that's on your `PATH`.
The menu app also opens at login.

To build from source instead, install Go 1.25 or later and Xcode 16 or later,
then run:

```sh
git clone https://github.com/joshp123/codex-switch.git
cd codex-switch
deploy/install.sh
```

The source build is signed for this Mac only. It uses the same proxy installer
as the downloaded app.

**2. Sign in to each account.** Each `login` opens the normal Codex sign-in in
your browser. Use a different account each time.

```sh
codex-switch login
codex-switch login
codex-switch accounts
```

```text
Codex 1	account-1	you@example.com
Codex 2	account-2	you+work@example.com
```

**3. Point Codex at it.** Run `codex-switch print-codex-config`. It prints one
line:

```toml
openai_base_url = "http://127.0.0.1:2455/client/<private key>/backend-api/codex"
```

Put it at the top of `~/.codex/config.toml`, above any `[section]`, and restart
Codex. The private key in the URL stops web pages and other apps from using your
accounts through the proxy. To go back to OpenAI directly, delete the line and
restart Codex.

For Pi, put the same URL in `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "openai-codex": {
      "baseUrl": "<the URL from print-codex-config>",
      "apiKey": "eyJhbGciOiJub25lIn0.eyJodHRwczovL2FwaS5vcGVuYWkuY29tL2F1dGgiOnsiY2hhdGdwdF9hY2NvdW50X2lkIjoiY29kZXgtc3dpdGNoIn19.codex-switch"
    }
  }
}
```

The `apiKey` is a placeholder: Pi won't start without one, and the proxy swaps
in the real account's sign-in.

**4. Check it works.**

```sh
codex-switch status
```

It shows each account's allowance, whether it can take new work (and why not),
and which accounts recent tasks went to. The log at
`~/Library/Application Support/codex-switch/proxy.log` records every assignment,
move, wait and rejected request. It never records prompts or responses.

### Claude subscriptions

Sign in once per Claude account:

```sh
codex-switch login --claude
codex-switch login --claude
codex-switch accounts
```

Claude sign-in opens Anthropic's subscription login in your browser and
finishes by itself when you approve it. Codex Switch stores and refreshes the
tokens in its own data folder. It leaves Claude Code's login and keychain alone.

For the Claude app's Code tab and Claude Code, run:

```sh
codex-switch print-claude-config
```

Merge the printed `env` entries into `~/.claude/settings.json`, keeping your
other settings:

```json
{
  "env": {
    "HTTPS_PROXY": "http://codex-switch:<private key>@127.0.0.1:2455",
    "NODE_EXTRA_CA_CERTS": "<data directory>/claude-proxy-ca.pem"
  }
}
```

New Claude Code sessions, including the Claude app's, pick it up straight away.
Keep Claude signed in to your normal account. Model requests use the pool; settings, plugins and Remote Control use
your own sign-in. The API address stays at `https://api.anthropic.com`.

Codex Switch creates a local certificate authority in its data directory. Only
Claude Code trusts it, through `NODE_EXTRA_CA_CERTS`; it is never added to the
macOS keychain or system trust store. The forward proxy decrypts requests to
`api.anthropic.com` and sends only `POST /v1/messages` through the pool. All other
hosts pass through as encrypted tunnels.

For Pi, run `codex-switch print-claude-config --pi` and merge the printed
`anthropic` provider into `~/.pi/agent/models.json`:

```json
{
  "providers": {
    "anthropic": {
      "baseUrl": "http://127.0.0.1:2455/client/<private key>/anthropic",
      "apiKey": "sk-ant-oat01-codex-switch"
    }
  }
}
```

The dummy key selects Pi's Claude subscription protocol; the proxy replaces it
with the chosen account's token. If Pi has a saved Anthropic login, its
credentials take precedence. Use Pi's `/logout` for Anthropic so it uses the
pool entry. Keep any Claude system-prompt extension you normally use enabled.
See [Pi's model settings](https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/models.md).

Claude limits show the five-hour session, the week, and each model's weekly
window. A model-specific limit applies to that model; other models can keep
using the account. Forked sessions stay with their parent's account while it
can serve the requested model, preserving the prompt cache.

## How routing works

- New tasks go to the account with the most weekly allowance left. Every
  sub-agent counts as its own task.
- A task stays on its account until its provider says that account has run out.
  Its next request then goes to another account.
- If a provider rejects a request before any work starts, the client retries
  as usual, and the retry lands on another account. The proxy never resends
  anything itself.
- If no account can run the model you asked for, the request waits until one
  can. The proxy never swaps your model for a different one.

Add an account any time with `codex-switch login` or `login --claude`, and
remove one with `codex-switch remove "Codex 2"`. No restart needed.

Keep allowance for an account you also use directly with
`codex-switch reserve "Claude 1" 15 10`. When any current allowance window has
15% or less left, new tasks prefer another account while pinned tasks stay.
At 10% or less, new work and the next turn of pinned tasks use another account.
The reserves are off by default; use `codex-switch reserve "Claude 1" off` to
clear both.

## How updates work

The app checks for signed, notarised releases every hour and installs them as
they arrive. The proxy switches only when a model request has arrived in the
last two minutes. Updates run one at a time, and the newest build wins.

The new proxy checks the signed-in accounts, then takes new connections. The
old proxy keeps its connections and stays ready for five minutes, or until the
new proxy has completed a reply of its own if that takes longer. If the new
proxy crashes or fails three requests during that time, it steps aside, the old
proxy continues, and that build is skipped; `codex-switch status` shows skipped
builds. If a provider cannot be read, or 25 minutes pass with no completed
reply, the new proxy steps aside and the switch is tried again.

The old proxy then stops accepting connections and finishes its work. Model
replies finish. Idle connections close at once. After a minute, Claude's event
streams and any tunnel with no traffic close too, and clients reopen them on
the new proxy. Tunnels still moving data carry on. Anything open 30 minutes
after the switch began closes.

To return to the previous proxy by hand, run `deploy/deploy.sh back` from a
checkout or from the app's `Contents/Resources/deploy` directory. The release
you leave is recorded as skipped.

For a source install, update the checkout and run `deploy/install.sh` again.

Maintainers: [prepare, verify and publish a release](docs/releases.md).

## Uninstall

First point your clients back at their providers: delete `openai_base_url` from
`~/.codex/config.toml`; remove the two Claude `env` entries above from
`~/.claude/settings.json`; remove the `openai-codex` and `anthropic` pool
entries from Pi's `models.json`, then restart the clients. Then:

```sh
/bin/sh /Applications/CodexSwitchMenu.app/Contents/Resources/deploy/uninstall.sh
```

The proxy finishes the requests it has already accepted and stops. The script
removes both launchd jobs, the `codex-switch` command and the menu app. Your
sign-ins and usage history stay in `~/Library/Application Support/codex-switch`;
move that folder to the Trash to remove them too.

## Compared with the alternatives

| | What it is | Lines of code |
| --- | --- | ---: |
| **Codex Switch** | Account pool, menu bar app, per-task usage, updates that don't drop work | **about 11,000** (Go and Swift) |
| [codex-lb](https://github.com/Soju06/codex-lb) | Account pool with a web dashboard; retries and replays requests inside the proxy | about 226,000 (Python and TypeScript) |
| [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) | Gateway for lots of AI providers, translating between their APIs | about 232,000 (Go) |
| [opencodex](https://github.com/lidge-jun/opencodex) | Proxy from Codex and Claude Code to any model provider | about 504,000 (TypeScript) |

The small `auth.json` switchers are simpler, but they switch accounts by
restarting Codex, which kills every running agent.

Codex Switch is small because it does one thing: pool subscription accounts for
one person on one Mac. Everything else is left to the clients. It has six direct
dependencies.

Line counts exclude tests, generated and vendored code, comments and blank
lines. Codex Switch was measured with [scc](https://github.com/boyter/scc) on
16 October 2026; the alternatives were measured on 11 October 2026.

## Local data

Everything lives in `~/Library/Application Support/codex-switch`: your account
sign-ins, the private URL key, which task is on which account, and token counts.
No prompts, responses or raw traffic are stored. See [SECURITY.md](SECURITY.md).

## Contributing

This is a personal project. Pull requests are switched off. Issues are open, but
I'll probably ignore them.

## Credits

The OpenAI and Claude marks in the menu and much of the menu's design come from
[OpenUsage](https://github.com/robinebers/openusage) by Robin Ebers, used under
the MIT licence (copyright 2026 Robin Ebers).

Codex Switch is not affiliated with OpenAI or Anthropic. OpenAI, ChatGPT and
Codex are trademarks of OpenAI. Claude is a trademark of Anthropic.

## Licence

MIT. See [LICENSE](LICENSE).
