import Foundation
import SwiftUI
import XCTest
@testable import CodexSwitchMenu

final class FormattingTests: XCTestCase {
    private let locale = Locale(identifier: "en_GB")
    private let utc = TimeZone(secondsFromGMT: 0)!

    func testPrivacyMaskKeepsEmailShapeWithoutRevealingAddress() {
        XCTAssertEqual(
            UsageFormatting.privateEmail("alice@example.com"),
            "a•••••@•••••.com"
        )
        XCTAssertEqual(UsageFormatting.privateEmail("account-1"), "account-1")
    }

    func testResetsAreClockTimesInTheMacsFormat() throws {
        let now = try Date.ISO8601FormatStyle().parse("2026-10-11T15:00:00Z")
        let tonight = try Date.ISO8601FormatStyle().parse("2026-10-11T23:36:00Z")
        let saturday = try Date.ISO8601FormatStyle().parse("2026-10-17T05:52:00Z")
        let november = try Date.ISO8601FormatStyle().parse("2026-11-08T11:20:00Z")
        let utc = TimeZone(identifier: "UTC")!

        XCTAssertEqual(UsageFormatting.resetLabel(tonight, now: now, locale: locale, timeZone: utc), "Resets Sun 11, 23:36")
        XCTAssertEqual(UsageFormatting.resetLabel(saturday, now: now, locale: locale, timeZone: utc), "Resets Sat 17, 05:52")
        XCTAssertEqual(UsageFormatting.resetLabel(november, now: now, locale: locale, timeZone: utc), "Resets Sun 8, 11:20")
    }

    func testStoredResetsNameTheirKindCountAndExpiry() throws {
        let now = try Date.ISO8601FormatStyle().parse("2026-11-09T21:00:00Z")
        func reset(_ kind: CSSwitch_ResetKind, _ count: UInt32, expires: String) throws -> CSSwitch_StoredReset {
            var reset = CSSwitch_StoredReset()
            reset.kind = kind
            reset.count = count
            reset.expiresAt = .init(date: try Date.ISO8601FormatStyle().parse(expires))
            return reset
        }
        var claude = CSSwitch_AllowanceObservation()
        claude.availableResetCredits = 3
        claude.storedResets = [try reset(.fable, 3, expires: "2027-03-09T07:59:00Z")]
        XCTAssertEqual(UsageFormatting.storedResets(claude, now: now, locale: locale, timeZone: utc), ["3 Fable resets expiring 9 Mar 2027"])

        var codex = CSSwitch_AllowanceObservation()
        codex.storedResets = [try reset(.weekly, 1, expires: "2026-11-14T09:02:00Z"), try reset(.weekly, 1, expires: "2026-12-02T17:55:00Z")]
        XCTAssertEqual(UsageFormatting.storedResets(codex, now: now, locale: locale, timeZone: utc), ["2 weekly resets, the first expiring 14 Nov"])

        // A proxy that sends no list sends only how many the account holds.
        var countOnly = CSSwitch_AllowanceObservation()
        countOnly.availableResetCredits = 2
        XCTAssertEqual(UsageFormatting.storedResets(countOnly, now: now, locale: locale, timeZone: utc), ["2 stored resets"])
        XCTAssertEqual(UsageFormatting.storedResets(CSSwitch_AllowanceObservation(), now: now, locale: locale, timeZone: utc), [])
    }

    func testAmountsUseThreeSignificantFigures() {
        XCTAssertEqual(UsageFormatting.tokens(946_491, locale: locale), "946K")
        XCTAssertEqual(UsageFormatting.tokens(34_138_059, locale: locale), "34.1M")
        XCTAssertEqual(UsageFormatting.tokens(6_883_276_314, locale: locale), "6.88B")
        XCTAssertEqual(UsageFormatting.tokens(999_600, locale: locale), "1.00M")
        XCTAssertEqual(UsageFormatting.tokens(512, locale: locale), "512")
        XCTAssertEqual(UsageFormatting.cost(4_990_000_000, locale: locale), "$4.99")
        XCTAssertEqual(UsageFormatting.cost(45_110_000_000, locale: locale), "$45.1")
        XCTAssertEqual(UsageFormatting.cost(647_960_000_000, locale: locale), "$648")
        XCTAssertEqual(UsageFormatting.cost(1_767_190_000_000, locale: locale), "$1,767")
        XCTAssertEqual(UsageFormatting.cost(0, locale: locale), "$0")
    }

    func testFormattingUsesSuppliedNumberSymbols() {
        XCTAssertEqual(UsageFormatting.tokens(2_090_000_000, locale: Locale(identifier: "nl_NL")), "2,09B")
    }

    @MainActor
    func testMenuBarShowsEachAccountNumberAndRoundedAllowance() {
        let layout = StatusStripRenderer.layout(for: [
            MenuBarAccountValue(provider: .chatgpt, number: 1, freePercent: 74.6),
            MenuBarAccountValue(provider: .chatgpt, number: 2, freePercent: 25.4),
        ])

        XCTAssertEqual(layout.badges, ["1", "2"])
        XCTAssertEqual(layout.values, ["75%", "25%"])
    }
}
