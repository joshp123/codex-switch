import SwiftUI

struct UsageSummaryCard: View {
    let presentation: UsagePresentation?
    let selectedPeriod: OverviewSnapshot.ReportingPeriodKind
    let selectedMetric: UsageMetric
    let enabledPeriods: Set<OverviewSnapshot.ReportingPeriodKind>
    let enabledMetrics: Set<UsageMetric>
    let selectPeriod: (OverviewSnapshot.ReportingPeriodKind) -> Void
    let selectMetric: (UsageMetric) -> Void
    @Environment(\.panelDensity) private var density

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(alignment: .firstTextBaseline) {
                MetricMenu(
                    selection: selectedMetric,
                    enabledMetrics: enabledMetrics,
                    select: selectMetric
                )
                Spacer()
            }
            UsagePeriodPicker(
                periods: OverviewSnapshot.ReportingPeriodKind.allCases,
                enabledPeriods: enabledPeriods,
                selection: selectedPeriod,
                select: selectPeriod
            )
            if let presentation {
                UsageBreakdown(
                    presentation: presentation,
                    selectedMetric: selectedMetric
                )
            }
        }
        .padding(density.cardInset)
        .frame(maxWidth: .infinity, alignment: .leading)
        .cardSurface()
    }

}

/// The ring of the period's split by account, with one legend line per
/// account beside it. Names keep their full width.
private struct UsageBreakdown: View {
    let presentation: UsagePresentation
    let selectedMetric: UsageMetric
    @Environment(\.panelDensity) private var density

    var body: some View {
        if let total {
            let shares = shares
            HStack(spacing: 18) {
                BreakdownRing(
                    shares: shares,
                    total: ringFormat(total),
                    accessibilityLabel: "\(selectedMetric.title) \(format(total))"
                )
                VStack(alignment: .leading, spacing: density.legendGap) {
                    ForEach(shares) { share in
                        HStack(spacing: 7) {
                            AccountMarker(colour: UsageTheme.accountColour(share.provider, share.accountNumber))
                            Text(share.label)
                                .font(UsageTheme.detailFont)
                                .fixedSize()
                            Spacer(minLength: 8)
                            Text(value(share))
                                .font(UsageTheme.detailEmphasisFont)
                                .foregroundStyle(.secondary)
                                .monospacedDigit()
                                .fixedSize()
                        }
                    }
                }
                .frame(maxWidth: .infinity, alignment: .leading)
            }
        }
    }

    private var shares: [UsagePresentation.Share] {
        switch selectedMetric {
        case .cost: presentation.accountCostShares
        case .tokens: presentation.accountTokenShares
        }
    }

    private var total: Int64? { presentation.total(selectedMetric) }

    private func ringFormat(_ amount: Int64) -> String {
        switch selectedMetric {
        case .cost: UsageFormatting.ringCost(amount)
        case .tokens: UsageFormatting.tokens(amount)
        }
    }

    private func format(_ amount: Int64) -> String {
        switch selectedMetric {
        case .cost: UsageFormatting.cost(amount)
        case .tokens: UsageFormatting.tokens(amount)
        }
    }

    private func value(_ share: UsagePresentation.Share) -> String {
        switch selectedMetric {
        case .cost: share.costNanoUSD.map { UsageFormatting.cost($0) } ?? ""
        case .tokens: UsageFormatting.tokens(share.tokens)
        }
    }
}

private struct MetricMenu: View {
    let selection: UsageMetric
    let enabledMetrics: Set<UsageMetric>
    let select: (UsageMetric) -> Void

    var body: some View {
        Menu {
            ForEach(UsageMetric.allCases.filter { enabledMetrics.contains($0) }) { metric in
                Button {
                    select(metric)
                } label: {
                    if metric == selection {
                        Label(metric.title, systemImage: "checkmark")
                    } else {
                        Text(metric.title)
                    }
                }
            }
        } label: {
            HStack(spacing: 4) {
                Text(selection.title)
                    .font(UsageTheme.labelFont)
                    .foregroundStyle(.primary)
                Image(systemName: "chevron.down")
                    .font(.system(size: 9, weight: .semibold))
                    .foregroundStyle(.secondary)
            }
        }
        .menuStyle(.button)
        .buttonStyle(.plain)
        .fixedSize()
        .accessibilityLabel("Usage metric")
        .accessibilityValue(Text(selection.title))
    }
}

private struct BreakdownRing: View {
    @Environment(\.panelDensity) private var density
    let shares: [UsagePresentation.Share]
    let total: String
    let accessibilityLabel: String

    var body: some View {
        ZStack {
            ForEach(CostRingArcLayout.arcs(from: shares)) { arc in
                RingSectorShape(
                    startFraction: arc.startFraction,
                    endFraction: arc.endFraction
                )
                .fill(UsageTheme.accountColour(arc.provider, arc.accountNumber))
            }
            Text(total)
                .font(.system(size: 14, weight: .semibold))
                .monospacedDigit()
                .minimumScaleFactor(0.6)
                .lineLimit(1)
                .padding(.horizontal, 24)
        }
        .frame(width: density.ringSize, height: density.ringSize)
        .accessibilityElement(children: .combine)
        .accessibilityLabel(accessibilityLabel)
    }
}
