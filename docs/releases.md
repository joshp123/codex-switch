<!-- Written by AI on 2026.10.17 by model GPT-6.1 Sol. -->

# Releases

Run these commands in `devenv shell` on the release Mac. A release needs a
Developer ID Application signing identity, GitHub CLI authentication, and the
App Store Connect team key already used by OpenTrawl. The scripts reference
that key in `~/.config/opentrawl/release/`; they do not copy it.

## Set up once

```sh
scripts/release/setup-release-credentials --create-sparkle-key
git add deploy/SparklePublicKey
git commit -m 'Set the Sparkle release public key'
```

Back up `~/.config/codex-switch/release/sparkle-ed25519-private-key`. Existing
installations trust its public key. On a replacement release Mac, restore the
private key and run setup without `--create-sparkle-key`.

## Prepare and check

Start from a clean checkout of the revision to release. Use a new version and
an output directory that does not exist:

```sh
scripts/release/prepare-release --version 0.2.0 --output "$PWD/dist/release-0.2.0"
scripts/release/verify-release --revision "$(git rev-parse HEAD)" dist/release-0.2.0
```

Preparation builds the app and its proxy, signs both and Sparkle, submits the
app to Apple, staples its notarisation ticket, then signs the ZIP with the
Codex Switch Sparkle key. It produces the exact ZIP, appcast and checksums that
publication uploads. It does not publish to GitHub.

The default build number is the source revision count. The app and bundled
proxy carry the same number. Use `--build INTEGER` for acceptance releases;
every installed update needs a larger build number.
`--identity` selects a Developer ID Application identity when several exist.
`--notes FILE` supplies release notes.

## Exercise an installed update

Use two clean revisions that both contain the updater and trust the same
Sparkle public key. The target must contain a different proxy revision, so the
check exercises a handover as well as Sparkle replacement.

Use the same isolated installation settings for both builds. Choose unused
ports and keep the root outside Applications and the normal data directory:

```sh
export CODEX_SWITCH_INSTALL_ROOT="$PWD/dist/update-check/home"
export CODEX_SWITCH_LAUNCHD_LABEL=dev.codex-switch.update-check
export CODEX_SWITCH_LISTEN=127.0.0.1:24681
```

1. At the older revision, prepare a source release with
   `--feed-url http://127.0.0.1:24682/appcast.xml` and a lower `--build`.
2. At the target revision, prepare the next version into a separate directory
   with the same settings and a larger build number. Serve that directory with
   `python3 -m http.server 24682 --bind 127.0.0.1 --directory DIR`.
3. Unpack the source ZIP into `dist/update-check/installed` and open that copy
   of the app. It installs an empty proxy pool in the isolated root, under the
   separate launchd label. It does not register itself to open at login.
4. Let the app check for and install the update. Send model work through the
   isolated proxy, then check the app version and the proxy's `deploy-status`
   build number. Check `proxy-install.log` and `proxy.log` under the isolated
   root: the new proxy must prove itself and the old one must drain and exit.
5. Uninstall the isolated copy with its bundled script, retaining the settings:
   `HOME="$CODEX_SWITCH_INSTALL_ROOT" CODEX_SWITCH_MENU_APP="$PWD/dist/update-check/installed/CodexSwitchMenu.app" sh dist/update-check/installed/CodexSwitchMenu.app/Contents/Resources/deploy/uninstall.sh`.
   Quit the test HTTP server and check that the isolated label and processes
   are gone.
6. Unset the three installation variables before preparing the final candidate
   with the default production feed. Isolated and loopback candidates cannot
   be published.

Run `scripts/dev/system-test` too. It uses real clients/accounts in an isolated
installation and checks admission, draining and reopening the same bundle
without restarting the proxy. It does not drive Sparkle or prove a signed
first installation. Check a first open on a Mac user account without an
existing proxy: it should create the launchd job, command link and working
menu without changing any client's configuration.

## Publish

After the real checks, with the release revision on current `origin/main`:

```sh
scripts/release/publish-release --candidate dist/release-0.2.0 --confirm v0.2.0
```

Publication verifies the candidate again and uploads its exact bytes to one
GitHub Release. Installed apps read the appcast attached to the latest release.
