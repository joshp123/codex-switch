import SwiftUI

/// The largest named tasks for the period, behind one row that opens in
/// place. Where spend went by task is an occasional question, so the row
/// starts closed and the choice is kept.
struct RoutedTasksSection: View {
    let snapshot: CSSwitch_MenuSnapshot
    let period: CSSwitch_ReportingPeriod?
    let selectedPeriod: OverviewSnapshot.ReportingPeriodKind
    let selectedMetric: UsageMetric
    /// How many rows the panel has room for when open; the list shows the largest tasks that fit.
    let maximumRows: Int
    @Binding var expanded: Bool
    @Environment(\.panelDensity) private var density

    static let mostRows = 10
    private static let rowHeight: CGFloat = 17
    private static let rowGap: CGFloat = 4

    /// The card's height with this many rows open; the panel uses it to give
    /// the list exactly the room the screen leaves.
    static func height(rows: Int, density: PanelDensity) -> CGFloat {
        density.cardInset * 2 + rowHeight + CGFloat(rows) * (rowHeight + rowGap)
    }

    /// At least one row when open, so opening always shows something.
    static func rows(fitting room: CGFloat, density: PanelDensity) -> Int {
        let rows = Int((room - height(rows: 0, density: density)) / (rowHeight + rowGap))
        return min(max(rows, 1), mostRows)
    }

    /// Whether the period has any task row that will be displayed.
    static func hasRows(snapshot: CSSwitch_MenuSnapshot, period: CSSwitch_ReportingPeriod?) -> Bool {
        for task in period?.taskUsage.tasks ?? [] where task.hasTitle && !task.title.isEmpty {
            if task.accountKeys.contains(where: { snapshot.account(key: $0) != nil }) {
                return true
            }
        }
        return false
    }

    /// Tasks that share a name and provider, such as repeated `codex exec`
    /// runs in one repository, make one row.
    private var rows: [TaskRow] {
        var rowsByID: [String: TaskRow] = [:]
        for task in period?.taskUsage.tasks ?? [] where task.hasTitle && !task.title.isEmpty {
            let accounts = task.accountKeys.compactMap(snapshot.account(key:))
            guard let provider = accounts.first?.menuProvider else { continue }
            let title = firstLine(task.title)
            let id = "\(provider.rawValue) \(title)"
            var row = rowsByID[id] ?? TaskRow(id: id, provider: provider, title: title)
            row.tokens += task.tokens
            row.costNanoUSD += task.costNanoUsd
            row.accountKeys.formUnion(accounts.map(\.key))
            row.lastCompletedAt = max(row.lastCompletedAt, task.lastCompletedAt.date)
            rowsByID[id] = row
        }
        let ranked = rowsByID.values.sorted { left, right in
            let leftAmount = selectedMetric == .tokens ? left.tokens : left.costNanoUSD
            let rightAmount = selectedMetric == .tokens ? right.tokens : right.costNanoUSD
            if leftAmount != rightAmount { return leftAmount > rightAmount }
            return left.lastCompletedAt > right.lastCompletedAt
        }
        return Array(ranked.prefix(Self.mostRows))
    }

    var body: some View {
        let rows = rows
        if !rows.isEmpty {
            let showsProvider = snapshot.providers.count > 1
            // Account markers sit in fixed columns, account 1 first for every
            // provider, so the values line up down the list.
            let slotsByProvider = Dictionary(grouping: rows.flatMap { row in row.accountKeys.map { (row.provider, $0) } }) { $0.0 }
                .mapValues { pairs in
                    Set(pairs.map(\.1)).compactMap(snapshot.account(key:)).sorted { $0.accountNumber < $1.accountNumber }
                }
            let slotCount = slotsByProvider.values.map(\.count).max() ?? 0
            VStack(alignment: .leading, spacing: Self.rowGap) {
                Button {
                    expanded.toggle()
                } label: {
                    HStack(alignment: .firstTextBaseline, spacing: 5) {
                        Text("Top tasks")
                            .font(UsageTheme.labelFont)
                        Image(systemName: expanded ? "chevron.down" : "chevron.right")
                            .font(.system(size: 9, weight: .semibold))
                            .foregroundStyle(.secondary)
                        Spacer()
                        Text(selectedPeriod.title)
                            .font(UsageTheme.detailFont)
                            .foregroundStyle(.secondary)
                    }
                    .frame(height: Self.rowHeight)
                    .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityLabel("Top tasks")
                .accessibilityValue(expanded ? "Open" : "Closed")
                if expanded {
                    ForEach(rows.prefix(maximumRows)) { row in
                        HStack(spacing: 7) {
                            if showsProvider {
                                ProviderMark(provider: row.provider, size: 12)
                                    .foregroundStyle(.secondary)
                            }
                            Text(row.title)
                                .font(UsageTheme.detailFont)
                                .lineLimit(1)
                                .truncationMode(.tail)
                            Spacer(minLength: 6)
                            Text(selectedMetric == .tokens ? UsageFormatting.tokens(row.tokens) : UsageFormatting.cost(row.costNanoUSD))
                                .font(UsageTheme.detailEmphasisFont)
                                .foregroundStyle(.secondary)
                                .monospacedDigit()
                                .fixedSize()
                            HStack(spacing: 4) {
                                let slots = slotsByProvider[row.provider] ?? []
                                ForEach(slots, id: \.key) { account in
                                    let used = row.accountKeys.contains(account.key)
                                    HStack(spacing: 3) {
                                        AccountMarker(colour: snapshot.colour(of: account), size: 7)
                                        Text("\(account.accountNumber)")
                                            .font(UsageTheme.detailEmphasisFont)
                                            .monospacedDigit()
                                    }
                                    .frame(width: 20, alignment: .trailing)
                                    .opacity(used ? 1 : 0)
                                    .accessibilityElement(children: .ignore)
                                    .accessibilityLabel(account.poolName)
                                    .accessibilityHidden(!used)
                                }
                            }
                            .frame(width: CGFloat(slotCount) * 24 - 4, alignment: .leading)
                        }
                        .frame(height: Self.rowHeight)
                    }
                }
            }
            .padding(density.cardInset)
            .cardSurface()
        }
    }

    private func firstLine(_ title: String) -> String {
        title.split(whereSeparator: \.isNewline).first.map(String.init) ?? title
    }
}

private struct TaskRow: Identifiable {
    let id: String
    let provider: CSSwitch_Provider
    let title: String
    var tokens: Int64 = 0
    var costNanoUSD: Int64 = 0
    var accountKeys: Set<String> = []
    var lastCompletedAt: Date = .distantPast
}
