#!/bin/zsh

set -euo pipefail

package_dir=${0:A:h}
repo_dir=${package_dir:h:h}
swift_scratch=${CODEX_SWITCH_SWIFT_SCRATCH_PATH:-$package_dir/.build}
destination="$repo_dir/dist/staged/CodexSwitchStage.app"
result=staged
display_name="Codex Switch Stage"
bundle_identifier=com.josh.codex-switch-menu.stage
running_bundle_name=CodexSwitchStage.app
if (( $# > 0 )); then
    if (( $# != 3 )) || [[ $2 != --destination ]] ||
        [[ $1 != --stage && $1 != --live && $1 != --release ]]; then
        print -u2 "usage: $0 [--stage --destination ABSOLUTE_APP_PATH | --live --destination ABSOLUTE_APP_PATH | --release --destination ABSOLUTE_APP_PATH]"
        exit 2
    fi
    mode=$1
    [[ $3 == /* ]] || {
        print -u2 "destination must be an absolute path: $3"
        exit 2
    }
    [[ $3 != / && $3 == *.app ]] || {
        print -u2 "destination must name one app bundle: $3"
        exit 2
    }
    destination=$3
    if [[ $1 == --stage ]]; then
        [[ $destination == "$repo_dir/dist/"* ]] || {
            print -u2 "staging destination must be under $repo_dir/dist: $destination"
            exit 2
        }
    else
        result=installed
        display_name="Codex Switch"
        bundle_identifier=com.josh.codex-switch-menu
        running_bundle_name=CodexSwitchMenu.app
    fi
fi

stage_name=${${destination:t}%.app}
if [[ $result == staged && $stage_name != CodexSwitchStage ]]; then
    display_name="Codex Switch $stage_name"
    bundle_identifier="com.josh.codex-switch-menu.stage.${(L)stage_name}"
fi
installation_root=${CODEX_SWITCH_INSTALL_ROOT:-}
if [[ -n $installation_root ]]; then
    [[ $installation_root == /* && -n ${CODEX_SWITCH_LAUNCHD_LABEL:-} && -n ${CODEX_SWITCH_LISTEN:-} ]] || {
        print -u2 "isolated packaging requires an absolute install root, launchd label and listen address"
        exit 2
    }
    display_name="Codex Switch Isolated"
    bundle_identifier=com.josh.codex-switch-menu.isolated
fi
running_matching_app=$(
    /bin/ps -axo pid=,comm= | /usr/bin/awk -v executable="$destination/Contents/MacOS/CodexSwitchMenu" '$2 == executable {print $1 " " $2}'
)
[[ -z $running_matching_app ]] || {
    print -u2 "refusing to replace $running_bundle_name while process $running_matching_app is running"
    exit 1
}

version=${CODEX_SWITCH_VERSION:-0.1.0}
build_number=${CODEX_SWITCH_BUILD:-$(git -C "$repo_dir" rev-list --count HEAD)}
identity=${CODEX_SWITCH_SIGN_IDENTITY:--}
feed_url=${CODEX_SWITCH_FEED_URL:-https://github.com/joshp123/codex-switch/releases/latest/download/appcast.xml}
public_key=${CODEX_SWITCH_SPARKLE_PUBLIC_KEY:-}
if [[ ${mode:-} == --release ]]; then
    [[ $identity == "Developer ID Application:"* && -n $public_key && -n ${CODEX_SWITCH_VERSION:-} && -n ${CODEX_SWITCH_BUILD:-} ]] || {
        print -u2 "release packaging requires version, build, Developer ID identity and Sparkle public key"
        exit 2
    }
fi

staging_root=$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/codex-switch-menu.XXXXXX")
staged_app="$staging_root/CodexSwitchMenu.app"

cleanup() {
    if [[ -d $staging_root ]]; then
        /usr/bin/trash "$staging_root"
    fi
}
trap cleanup EXIT

/usr/bin/swift build --build-system native --package-path "$package_dir" --scratch-path "$swift_scratch" --force-resolved-versions -c release
bin_dir=$(/usr/bin/swift build --build-system native --package-path "$package_dir" --scratch-path "$swift_scratch" --force-resolved-versions -c release --show-bin-path)
executable="$bin_dir/CodexSwitchMenu"
resource_bundle="$bin_dir/CodexSwitchMenu_CodexSwitchMenu.bundle"

[[ -x $executable ]] || { print -u2 "missing executable: $executable"; exit 1; }
[[ -d $resource_bundle ]] || { print -u2 "missing resource bundle: $resource_bundle"; exit 1; }

/bin/mkdir -p "$staged_app/Contents/MacOS" "$staged_app/Contents/Resources/deploy" "$staged_app/Contents/Helpers" "$staged_app/Contents/Frameworks"
/bin/cp "$executable" "$staged_app/Contents/MacOS/CodexSwitchMenu"
/bin/cp -R "$resource_bundle" "$staged_app/Contents/Resources/"

sparkle_framework="$swift_scratch/artifacts/sparkle/Sparkle/Sparkle.xcframework/macos-arm64_x86_64/Sparkle.framework"
/usr/bin/ditto "$sparkle_framework" "$staged_app/Contents/Frameworks/Sparkle.framework"
CODEX_SWITCH_BUILD="$build_number" "$repo_dir/scripts/dev/build-proxy" "$staged_app/Contents/Helpers/codex-switch"
for script in deploy.sh installation.sh install-proxy.sh uninstall.sh; do
    /bin/cp "$repo_dir/deploy/$script" "$staged_app/Contents/Resources/deploy/"
done

plist="$staged_app/Contents/Info.plist"
/usr/bin/plutil -create xml1 "$plist"
/usr/bin/plutil -insert CFBundleDisplayName -string "$display_name" "$plist"
/usr/bin/plutil -insert CFBundleExecutable -string CodexSwitchMenu "$plist"
/usr/bin/plutil -insert CFBundleIdentifier -string "$bundle_identifier" "$plist"
/usr/bin/plutil -insert CFBundleName -string CodexSwitchMenu "$plist"
/usr/bin/plutil -insert CFBundlePackageType -string APPL "$plist"
/usr/bin/plutil -insert CFBundleShortVersionString -string "$version" "$plist"
/usr/bin/plutil -insert CFBundleVersion -string "$build_number" "$plist"
/usr/bin/plutil -insert LSMinimumSystemVersion -string 14.0 "$plist"
/usr/bin/plutil -insert LSUIElement -bool true "$plist"
/usr/bin/plutil -insert NSPrincipalClass -string NSApplication "$plist"

/usr/bin/plutil -insert GitCommit -string "$(git -C "$repo_dir" rev-parse HEAD)" "$plist"
/usr/bin/plutil -insert CSManagesProxy -bool $([[ $result == installed ]] && echo true || echo false) "$plist"
if [[ -n $installation_root ]]; then
    /usr/bin/plutil -insert CSInstallationRoot -string "$installation_root" "$plist"
    /usr/bin/plutil -insert CSLaunchdLabel -string "$CODEX_SWITCH_LAUNCHD_LABEL" "$plist"
    /usr/bin/plutil -insert CSListenAddress -string "$CODEX_SWITCH_LISTEN" "$plist"
fi
if [[ -n $public_key ]]; then
    /usr/bin/plutil -insert SUFeedURL -string "$feed_url" "$plist"
    /usr/bin/plutil -insert SUPublicEDKey -string "$public_key" "$plist"
    /usr/bin/plutil -insert SUEnableAutomaticChecks -bool true "$plist"
    # Hourly, Sparkle's shortest interval: a fix reaches every Mac within the hour.
    /usr/bin/plutil -insert SUScheduledCheckInterval -integer 3600 "$plist"
    # Updates install themselves; the app hands the proxy over blue-green.
    /usr/bin/plutil -insert SUAutomaticallyUpdate -bool true "$plist"
    /usr/bin/plutil -insert SUEnableSystemProfiling -bool false "$plist"
fi

sign_args=(--force --sign "$identity")
if [[ $identity != - ]]; then
    sign_args+=(--options runtime --timestamp)
fi
sparkle="$staged_app/Contents/Frameworks/Sparkle.framework/Versions/B"
/usr/bin/codesign "${sign_args[@]}" "$sparkle/XPCServices/Installer.xpc"
/usr/bin/codesign "${sign_args[@]}" --preserve-metadata=entitlements "$sparkle/XPCServices/Downloader.xpc"
/usr/bin/codesign "${sign_args[@]}" "$sparkle/Autoupdate"
/usr/bin/codesign "${sign_args[@]}" "$sparkle/Updater.app"
/usr/bin/codesign "${sign_args[@]}" "${sparkle:h:h}"
/usr/bin/codesign "${sign_args[@]}" --identifier codex-switch "$staged_app/Contents/Helpers/codex-switch"
/usr/bin/codesign "${sign_args[@]}" "$staged_app"
/usr/bin/codesign --verify --deep --strict "$staged_app"

/bin/mkdir -p "${destination:h}"
if [[ -e $destination ]]; then
    /usr/bin/trash "$destination"
fi
/bin/mv "$staged_app" "$destination"

print "$result $destination"
