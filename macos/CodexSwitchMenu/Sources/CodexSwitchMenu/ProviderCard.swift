import SwiftUI

/// A provider's accounts in one card, organised by allowance window: first
/// who the accounts are, one line each, then one block per window with one
/// grid row per account, so one window is read across every account at
/// once and a new account adds one row per block. Every row in every block
/// sits on the same grid, and every group on the same left edge.
struct ProviderSection: View {
    let snapshot: CSSwitch_MenuSnapshot
    let provider: CSSwitch_Provider
    let hideIdentity: Bool
    let now: Date
    @Environment(\.panelDensity) private var density

    var body: some View {
        let accounts = snapshot.accounts.filter { $0.menuProvider == provider }.sorted { $0.accountNumber < $1.accountNumber }
        VStack(alignment: .leading, spacing: 6) {
            ProviderHeader(provider: provider)
                .padding(.horizontal, density.cardInset)
            VStack(alignment: .leading, spacing: density.groupGap) {
                AccountIdentities(accounts: accounts, snapshot: snapshot, hideIdentity: hideIdentity)
                ForEach(IndicatorBlock.blocks(for: accounts, provider: provider)) { block in
                    VStack(alignment: .leading, spacing: density.blockTitleGap) {
                        Text(block.title)
                            .font(UsageTheme.labelFont)
                        Grid(alignment: .leading, horizontalSpacing: density.columnGap, verticalSpacing: density.rowGap) {
                            ForEach(block.rows, id: \.account.key) { row in
                                AllowanceGridRow(
                                    number: Int(row.account.accountNumber),
                                    colour: snapshot.colour(of: row.account),
                                    allowance: row.window,
                                    now: now
                                )
                            }
                        }
                    }
                }
            }
            .padding(density.cardInset)
            .frame(maxWidth: .infinity, alignment: .leading)
            .cardSurface()
        }
    }
}

/// One line per account: its colour, number, address (censored in privacy
/// mode, never cut), plan, stored resets, and the one state that keeps new
/// work off it when there is one, in the proxy's words. The dot sits on the
/// same edge as the dots in the rows below.
private struct AccountIdentities: View {
    let accounts: [CSSwitch_Account]
    let snapshot: CSSwitch_MenuSnapshot
    let hideIdentity: Bool
    @Environment(\.panelDensity) private var density

    var body: some View {
        // Columns, so plans and states line up under each other across the
        // provider's accounts: number, address, then the plan and any extras.
        Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: density.columnGap + 4, verticalSpacing: density.identityGap) {
            ForEach(accounts, id: \.key) { account in
                GridRow {
                    HStack(spacing: 3) {
                        AccountMarker(colour: snapshot.colour(of: account), size: 7)
                        Text("\(account.accountNumber)")
                            .font(density.rowEmphasisFont)
                            .monospacedDigit()
                    }
                    .frame(width: density.numberWidth, alignment: .leading)
                    Text(hideIdentity ? UsageFormatting.privateEmail(account.email) : account.email)
                        .fixedSize()
                    // One line; the panel widens to fit the longest.
                    Text(([account.planTitle] + resets(account) + [state(account)].compactMap { $0 }).joined(separator: " · "))
                        .fixedSize()
                }
                .font(density.identityFont)
                .foregroundStyle(.secondary)
            }
        }
    }

    /// "3 stored resets", or "not in pool" for an account whose plan cannot
    /// run the configured model.
    private func resets(_ account: CSSwitch_Account) -> [String] {
        var parts: [String] = []
        if account.allowance.hasAvailableResetCredits, account.allowance.availableResetCredits > 0 {
            let count = account.allowance.availableResetCredits
            parts.append(count == 1 ? "1 stored reset" : "\(count) stored resets")
        }
        if !account.offersConfiguredModel { parts.append("not in pool") }
        return parts
    }

    /// "no new work · 15% reserve", "held back · 10% reserve", or a limit, in
    /// the proxy's words, with when the limit ends in the Mac's time zone.
    private func state(_ account: CSSwitch_Account) -> String? {
        var state = account.usable ? account.reserveStatus : account.offersConfiguredModel ? account.unavailableReason : ""
        if !account.usable, account.offersConfiguredModel, account.hasUnavailableUntil {
            state += " until " + UsageFormatting.dayAndClock(account.unavailableUntil.date)
        }
        return state.isEmpty ? nil : state
    }
}

/// One allowance window across the provider's pool accounts.
struct IndicatorBlock: Identifiable {
    struct Row {
        let account: CSSwitch_Account
        let window: CSSwitch_AllowanceWindow
    }

    let id: String
    let title: String
    let rows: [Row]

    /// Shortest window first; a model's own limit follows the limit it sits
    /// inside. Accounts keep their number order within every block. An
    /// account that cannot run the configured model has no rows: its
    /// allowance is not the pool's, and its identity line says so.
    static func blocks(for accounts: [CSSwitch_Account], provider: CSSwitch_Provider) -> [IndicatorBlock] {
        var rowsByKey: [String: [Row]] = [:]
        var windowsByKey: [String: CSSwitch_AllowanceWindow] = [:]
        for account in accounts where account.hasAllowance && account.offersConfiguredModel {
            for window in account.allowance.windows {
                let key = "\(window.periodSeconds) \(window.modelID) \(window.modelName)"
                rowsByKey[key, default: []].append(Row(account: account, window: window))
                windowsByKey[key] = window
            }
        }
        return windowsByKey.sorted { left, right in
            (left.value.periodSeconds, left.value.modelID, left.value.modelName)
                < (right.value.periodSeconds, right.value.modelID, right.value.modelName)
        }.map { key, window in
            IndicatorBlock(id: key, title: window.title(for: provider), rows: rowsByKey[key] ?? [])
        }
    }
}

extension CSSwitch_AllowanceWindow {
    var freePercent: Double { 100 - usedPercent }

    /// "Session", "Weekly", or the model the limit belongs to, such as "Fable".
    func title(for provider: CSSwitch_Provider) -> String {
        guard modelID.isEmpty && modelName.isEmpty else { return modelName.isEmpty ? modelID : modelName }
        return switch periodSeconds {
        case 18_000 where provider == .claude: "Session"
        case 604_800: "Weekly"
        case let seconds where seconds % 86_400 == 0: "\(seconds / 86_400)-day"
        case let seconds where seconds % 3_600 == 0: "\(seconds / 3_600)-hour"
        default: "\(periodSeconds / 60)-minute"
        }
    }

    /// Nothing left to use before the reset.
    var isEmpty: Bool { usedPercent >= 100 }
}

extension CSSwitch_Account {
    /// "Pro", "Max 20x".
    var planTitle: String { plan.prefix(1).uppercased() + plan.dropFirst() }
}

/// One account's line in a window block: its colour and number, the bar,
/// what is left, and when it resets. Every figure is one the provider
/// states. The bar is red only when little or nothing is left: empty, or
/// under a tenth. Why an account takes no new work is on its identity line.
private struct AllowanceGridRow: View {
    let number: Int
    let colour: Color
    let allowance: CSSwitch_AllowanceWindow
    @Environment(\.panelDensity) private var density

    var body: some View {
        let isLow = allowance.isEmpty || allowance.freePercent < 10
        GridRow {
            HStack(spacing: 3) {
                AccountMarker(colour: colour, size: 7)
                Text("\(number)")
                    .font(density.rowEmphasisFont)
                    .monospacedDigit()
            }
            .frame(width: density.numberWidth, alignment: .leading)
            AllowanceBar(freeFraction: min(max(allowance.freePercent / 100, 0), 1), low: isLow, height: density.barHeight)
                .frame(maxWidth: .infinity)
            Text(allowance.isEmpty ? "Empty" : "\(UsageFormatting.percent(allowance.freePercent)) left")
                .font(density.rowEmphasisFont)
                .foregroundStyle(allowance.isEmpty ? AnyShapeStyle(UsageTheme.runOutColour) : AnyShapeStyle(.primary))
                .monospacedDigit()
                .lineLimit(1)
                .frame(width: density.percentWidth, alignment: .trailing)
            Text(resetText)
                .font(density.rowFont)
                .foregroundStyle(.secondary)
                .monospacedDigit()
                .lineLimit(1)
                .frame(width: density.timeWidth, alignment: .trailing)
        }
        .frame(height: density.rowHeight)
        .accessibilityElement(children: .combine)
        .accessibilityLabel("Account \(number): \(allowance.isEmpty ? "empty" : "\(UsageFormatting.percent(allowance.freePercent)) left"), \(resetText)")
    }

    /// A reset already past means the window refilled and has not started
    /// again, so there is no time to show.
    private var resetText: String {
        guard allowance.hasResetAt, allowance.resetAt.date > now else { return "" }
        return UsageFormatting.resetLabel(allowance.resetAt.date, now: now)
    }

    let now: Date
}

/// Full-width capsule of what is left: blue while the window lasts to its
/// reset, red when it is running out or nearly gone, empty when it is empty.
private struct AllowanceBar: View {
    let freeFraction: Double
    let low: Bool
    let height: CGFloat

    var body: some View {
        GeometryReader { geometry in
            ZStack(alignment: .leading) {
                Capsule().fill(.fill.secondary)
                Capsule()
                    .fill(low ? UsageTheme.runOutColour : UsageTheme.capacityColour)
                    .frame(width: freeFraction > 0 ? max(height, geometry.size.width * freeFraction) : 0)
            }
            .frame(height: height)
            .frame(maxHeight: .infinity, alignment: .center)
        }
        .accessibilityHidden(true)
    }
}
