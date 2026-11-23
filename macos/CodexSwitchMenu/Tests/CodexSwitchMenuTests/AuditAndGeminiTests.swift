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
}
