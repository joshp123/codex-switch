import Foundation
import SwiftProtobuf
import XCTest
@testable import CodexSwitchMenu

final class AuditAndGeminiTests: XCTestCase {
    private let locale = Locale(identifier: "en_GB")

    func testSnapshotURLRejectsNonLoopbackOrMissingPort() throws {
        let dir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString, directoryHint: .isDirectory)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }

        let client = SnapshotClient(dataDirectory: dir)

        try "http://127.0.0.1:2455/client/cap/codex-switch/menu-snapshot\n".write(to: client.urlFile, atomically: true, encoding: .utf8)
        XCTAssertEqual(try client.snapshotURL().host, "127.0.0.1")

        try "http://127.0.0.2:2455/client/cap/codex-switch/menu-snapshot\n".write(to: client.urlFile, atomically: true, encoding: .utf8)
        XCTAssertEqual(try client.snapshotURL().host, "127.0.0.2")

        try "http://127.+0.0.1:2455/client/cap/codex-switch/menu-snapshot\n".write(to: client.urlFile, atomically: true, encoding: .utf8)
        XCTAssertThrowsError(try client.snapshotURL())

        try "http://example.com:2455/client/cap/codex-switch/menu-snapshot\n".write(to: client.urlFile, atomically: true, encoding: .utf8)
        XCTAssertThrowsError(try client.snapshotURL())

        try "http://127.0.0.1/client/cap/codex-switch/menu-snapshot\n".write(to: client.urlFile, atomically: true, encoding: .utf8)
        XCTAssertThrowsError(try client.snapshotURL())
    }

    func testParseComposedAtHandlesFractionalAndWholeSeconds() {
        XCTAssertNotNil(SnapshotClient.parseComposedAt("2026-11-23T12:34:56.789123Z"))
        XCTAssertNotNil(SnapshotClient.parseComposedAt("2026-11-23T12:34:56Z"))
        XCTAssertNil(SnapshotClient.parseComposedAt("not-a-date"))
    }

    func testLaunchConfigurationIgnoresPSNFlag() throws {
        let config = try LaunchConfiguration(arguments: ["-psn_0_123456"])
        XCTAssertTrue(config.usesManagedDataDirectory)
        XCTAssertThrowsError(try LaunchConfiguration(arguments: ["--unknown-flag"]))
    }

    func testRingCostRoundsNearTenThousandThreshold() {
        XCTAssertEqual(UsageFormatting.ringCost(9_999_600_000_000, locale: locale), "$10.0K")
        XCTAssertEqual(UsageFormatting.ringCost(9_999_400_000_000, locale: locale), "$9,999")
    }

    @MainActor
    func testGeminiProviderIdentityAndWindowTitles() throws {
        XCTAssertEqual(CSSwitch_Provider.gemini.title, "Gemini")
        XCTAssertFalse(CSSwitch_Provider.gemini.isRouted)
        XCTAssertNotNil(CSSwitch_Provider.gemini.mark)

        var account = CSSwitch_Account()
        account.key = "gemini:local"
        account.accountNumber = 1
        account.plan = "Antigravity"
        account.provider = .gemini
        account.state = .ready
        account.offersConfiguredModel = true
        account.usable = false
        account.unavailableReason = "usage only"

        var allowance = CSSwitch_AllowanceObservation()
        allowance.observedAt = Google_Protobuf_Timestamp(date: Date())
        let reset = Google_Protobuf_Timestamp(date: Date().addingTimeInterval(3600))

        var w5h = CSSwitch_AllowanceWindow()
        w5h.periodSeconds = 18_000
        w5h.usedPercent = 8
        w5h.resetAt = reset

        var w7d = CSSwitch_AllowanceWindow()
        w7d.periodSeconds = 604_800
        w7d.usedPercent = 30
        w7d.resetAt = reset

        var w3p5h = CSSwitch_AllowanceWindow()
        w3p5h.periodSeconds = 18_000
        w3p5h.usedPercent = 0
        w3p5h.resetAt = reset
        w3p5h.modelID = "3p"
        w3p5h.modelName = "Claude & GPT"

        var w3p7d = CSSwitch_AllowanceWindow()
        w3p7d.periodSeconds = 604_800
        w3p7d.usedPercent = 6
        w3p7d.resetAt = reset
        w3p7d.modelID = "3p"
        w3p7d.modelName = "Claude & GPT"

        allowance.windows = [w5h, w7d, w3p5h, w3p7d]
        account.allowance = allowance

        let blocks = IndicatorBlock.blocks(for: [account], provider: .gemini)
        XCTAssertEqual(blocks.map(\.title), ["5-hour", "Claude & GPT · 5-hour", "Weekly", "Claude & GPT · Weekly"])

        let barValues = MenuBarAccountValue.values(for: [account])
        XCTAssertEqual(barValues, [MenuBarAccountValue(provider: .gemini, number: 1, freePercent: 70)])

        var failed = account
        failed.state = .usageCheckFailed
        XCTAssertEqual(MenuBarAccountValue.values(for: [failed]), [])
    }

    @MainActor
    func testOutsideClickDismissIgnoresStatusButtonWindowsAndPanel() {
        let primaryWindowFrame = NSRect(x: 1000, y: 946, width: 186, height: 37)
        let secondaryWindowFrame = NSRect(x: 2400, y: 1043, width: 186, height: 37)
        let panelFrame = NSRect(x: 700, y: 100, width: 500, height: 840)

        // Click in the outer 7.5pt top/bottom and 8pt side padding of a 37pt menu bar item.
        XCTAssertFalse(StatusBarController.shouldDismissForOutsideClick(
            at: NSPoint(x: 1002, y: 948),
            statusItemFrames: [primaryWindowFrame, secondaryWindowFrame],
            panelFrame: panelFrame
        ))
        // Click on a secondary display's replicated status bar window.
        XCTAssertFalse(StatusBarController.shouldDismissForOutsideClick(
            at: NSPoint(x: 2450, y: 1060),
            statusItemFrames: [primaryWindowFrame, secondaryWindowFrame],
            panelFrame: panelFrame
        ))
        // Click inside panel.
        XCTAssertFalse(StatusBarController.shouldDismissForOutsideClick(
            at: NSPoint(x: 800, y: 500),
            statusItemFrames: [primaryWindowFrame, secondaryWindowFrame],
            panelFrame: panelFrame
        ))
        // True outside click.
        XCTAssertTrue(StatusBarController.shouldDismissForOutsideClick(
            at: NSPoint(x: 200, y: 500),
            statusItemFrames: [primaryWindowFrame, secondaryWindowFrame],
            panelFrame: panelFrame
        ))
    }

    @MainActor
    func testToggleReopenSuppressedAfterSameClickOrImmediateExternalDismiss() {
        let now = ContinuousClock.now
        let clickEvent = StatusBarController.EventIdentity(eventNumber: 42, timestamp: 100.5)
        let sameClickLater = StatusBarController.EventIdentity(eventNumber: 42, timestamp: 100.52)
        let sameTimestampZeroEventNum = StatusBarController.EventIdentity(eventNumber: 0, timestamp: 100.51)
        let nextClick = StatusBarController.EventIdentity(eventNumber: 43, timestamp: 101.2)

        let recentDismiss = StatusBarController.ExternalDismiss(event: clickEvent, dismissedAt: now)
        XCTAssertTrue(StatusBarController.shouldIgnoreToggleReopen(
            currentEvent: sameClickLater, externalDismiss: recentDismiss, now: now + .milliseconds(400)
        ))
        XCTAssertTrue(StatusBarController.shouldIgnoreToggleReopen(
            currentEvent: sameTimestampZeroEventNum, externalDismiss: recentDismiss, now: now + .milliseconds(400)
        ))
        XCTAssertTrue(StatusBarController.shouldIgnoreToggleReopen(
            currentEvent: nextClick, externalDismiss: recentDismiss, now: now + .milliseconds(100)
        ))
        XCTAssertFalse(StatusBarController.shouldIgnoreToggleReopen(
            currentEvent: nextClick, externalDismiss: recentDismiss, now: now + .milliseconds(300)
        ))
        // Direct toggle close clears externalDismiss, so rapid reopen on next click works immediately.
        XCTAssertFalse(StatusBarController.shouldIgnoreToggleReopen(
            currentEvent: nextClick, externalDismiss: nil, now: now
        ))
    }

    @MainActor
    func testEventIdentityIgnoresKeyEventsWithoutCrashing() {
        let keyEvent = NSEvent.keyEvent(
            with: .keyDown,
            location: .zero,
            modifierFlags: [],
            timestamp: 42.0,
            windowNumber: 0,
            context: nil,
            characters: "\u{1b}",
            charactersIgnoringModifiers: "\u{1b}",
            isARepeat: false,
            keyCode: 53
        )
        XCTAssertNil(StatusBarController.EventIdentity(keyEvent))

        let mouseEvent = NSEvent.mouseEvent(
            with: .leftMouseDown,
            location: NSPoint(x: 10, y: 10),
            modifierFlags: [],
            timestamp: 42.5,
            windowNumber: 0,
            context: nil,
            eventNumber: 77,
            clickCount: 1,
            pressure: 1
        )
        XCTAssertEqual(
            StatusBarController.EventIdentity(mouseEvent),
            StatusBarController.EventIdentity(eventNumber: 77, timestamp: 42.5)
        )
    }

    @MainActor
    func testIsOwnWindowRecognizesReplicatedStatusBarWindowsAndMenus() {
        final class FakeStatusBarWindow: NSWindow {}
        final class FakeContextMenuWindow: NSWindow {}

        let panel = NSWindow(contentRect: .zero, styleMask: [.borderless], backing: .buffered, defer: true)
        let primaryStatus = FakeStatusBarWindow(contentRect: .zero, styleMask: [.borderless], backing: .buffered, defer: true)
        let replicatedStatus = FakeStatusBarWindow(contentRect: .zero, styleMask: [.borderless], backing: .buffered, defer: true)
        let menuWindow = FakeContextMenuWindow(contentRect: .zero, styleMask: [.borderless], backing: .buffered, defer: true)
        let otherWindow = NSWindow(contentRect: .zero, styleMask: [.titled], backing: .buffered, defer: true)

        XCTAssertTrue(StatusBarController.isOwnWindow(nil, panel: panel, statusButtonWindow: primaryStatus))
        XCTAssertTrue(StatusBarController.isOwnWindow(panel, panel: panel, statusButtonWindow: primaryStatus))
        XCTAssertTrue(StatusBarController.isOwnWindow(primaryStatus, panel: panel, statusButtonWindow: primaryStatus))
        XCTAssertTrue(StatusBarController.isOwnWindow(replicatedStatus, panel: panel, statusButtonWindow: primaryStatus))
        XCTAssertTrue(StatusBarController.isOwnWindow(menuWindow, panel: panel, statusButtonWindow: primaryStatus))
        XCTAssertFalse(StatusBarController.isOwnWindow(otherWindow, panel: panel, statusButtonWindow: primaryStatus))

        let onScreen = FakeStatusBarWindow(contentRect: NSRect(x: 1200, y: 1050, width: 140, height: 30), styleMask: [.borderless], backing: .buffered, defer: true)
        let offScreen = FakeStatusBarWindow(contentRect: NSRect(x: 0, y: -30, width: 140, height: 30), styleMask: [.borderless], backing: .buffered, defer: true)
        XCTAssertTrue(StatusBarController.anchorStatusBarWindow(currentEventWindow: onScreen, buttonWindow: offScreen) === onScreen)
        XCTAssertTrue(StatusBarController.anchorStatusBarWindow(currentEventWindow: offScreen, buttonWindow: primaryStatus) === primaryStatus)
        XCTAssertTrue(StatusBarController.anchorStatusBarWindow(currentEventWindow: otherWindow, buttonWindow: primaryStatus) === primaryStatus)
    }

    func testInitialPanelDensityAndGeminiWeeklyPace() {
        var snapshot = CSSwitch_MenuSnapshot()
        for idx in 1...2 {
            var a = CSSwitch_Account()
            a.key = "chatgpt:c\(idx)@example.com"
            a.accountNumber = UInt32(idx)
            a.provider = .chatgpt
            a.offersConfiguredModel = true
            snapshot.accounts.append(a)
        }
        var claude = CSSwitch_Account()
        claude.key = "claude:cl@example.com"
        claude.accountNumber = 1
        claude.provider = .claude
        claude.offersConfiguredModel = true
        snapshot.accounts.append(claude)

        var gemini = CSSwitch_Account()
        gemini.key = "gemini:local"
        gemini.accountNumber = 1
        gemini.provider = .gemini
        gemini.offersConfiguredModel = true
        snapshot.accounts.append(gemini)

        var burn = CSSwitch_ProviderBurn()
        burn.provider = .gemini
        burn.accountsPerWeek = 0.8
        snapshot.providerBurns = [burn]

        XCTAssertEqual(PanelDensity.initial(for: snapshot, maximumHeight: 929), .compact)
        XCTAssertEqual(PanelDensity.initial(for: snapshot, maximumHeight: 1400), .regular)
        XCTAssertEqual(
            WeeklyPace.lines(snapshot, locale: Locale(identifier: "en_US_POSIX")),
            ["Gemini: burning 0.8 accounts a week, 1 in the pool"]
        )
    }
}
