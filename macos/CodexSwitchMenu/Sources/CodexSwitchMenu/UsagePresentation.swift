import Foundation

enum UsageMetric: String, CaseIterable, Hashable, Identifiable, Sendable {
    case cost, tokens
    var id: String { rawValue }
    var title: LocalizedStringResource {
        switch self {
        case .cost: "API-equivalent cost"
        case .tokens: "Tokens"
        }
    }
}

struct UsagePresentation: Equatable, Sendable {
    let totalCostNanoUSD: Int64?
    let hasMultipleProviders: Bool
    let totalTokens: Int64
    let accountTokenShares: [Share]
    let accountCostShares: [Share]

    init?(snapshot: OverviewSnapshot, selectedPeriod: OverviewSnapshot.ReportingPeriodKind) {
        guard let period = snapshot.reportingPeriod(selectedPeriod), !period.accounts.isEmpty else { return nil }
        let hasMultipleProviders = snapshot.menuSnapshot.providers.count > 1
        self.hasMultipleProviders = hasMultipleProviders
        let accounts = snapshot.menuSnapshot.accounts
        // One slice per account, in the pool's order, so the legend reads
        // provider by provider. An account outside the pool for the configured
        // model is listed only if it spent something.
        let totals = accounts.compactMap { account -> (CSSwitch_Account, CSSwitch_AccountTotal)? in
            guard let total = period.accounts.first(where: { $0.accountKey == account.key }) else { return nil }
            if !account.offersConfiguredModel, total.tokens == 0, total.costNanoUsd == 0 { return nil }
            return (account, total)
        }
        totalTokens = totals.reduce(0) { $0 + $1.1.tokens }
        totalCostNanoUSD = totals.allSatisfy(\.1.hasCostNanoUsd) ? totals.reduce(0) { $0 + $1.1.costNanoUsd } : nil
        func shares(metric: UsageMetric, total: Int64) -> [Share] {
            var cursor = 0.0
            return totals.map { account, item in
                let start = cursor
                let amount = metric == .tokens ? item.tokens : item.costNanoUsd
                if total > 0 { cursor += Double(amount) / Double(total) }
                return Share(
                    id: account.key,
                    label: hasMultipleProviders ? account.poolName : "Account \(account.accountNumber)",
                    provider: account.menuProvider,
                    accountNumber: Int(account.accountNumber),
                    tokens: item.tokens,
                    costNanoUSD: item.hasCostNanoUsd ? item.costNanoUsd : nil,
                    startFraction: start,
                    endFraction: cursor
                )
            }
        }
        accountTokenShares = shares(metric: .tokens, total: totalTokens)
        accountCostShares = totalCostNanoUSD.map { shares(metric: .cost, total: $0) } ?? []
    }

    func total(_ metric: UsageMetric) -> Int64? {
        switch metric {
        case .cost: totalCostNanoUSD
        case .tokens: totalTokens
        }
    }

    struct Share: Equatable, Identifiable, Sendable {
        let id: String
        let label: String
        let provider: CSSwitch_Provider
        let accountNumber: Int
        let tokens: Int64
        let costNanoUSD: Int64?
        let startFraction: Double
        let endFraction: Double
    }
}
