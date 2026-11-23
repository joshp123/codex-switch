import Foundation

enum MenuBarDisplayMode: String, CaseIterable, Hashable, Identifiable, Sendable {
    case pooledAverage
    case miniBars
    case accountsLeft

    var id: String { rawValue }

    var title: String {
        switch self {
        case .pooledAverage: "Pooled average"
        case .miniBars: "Mini bars"
        case .accountsLeft: "Accounts left"
        }
    }

    static let storageKey = "menuBarDisplayMode"

    static func current(in defaults: UserDefaults = .standard) -> Self {
        defaults.string(forKey: storageKey).flatMap(Self.init(rawValue:)) ?? .pooledAverage
    }
}

struct MenuBarAccountValue: Equatable, Sendable {
    let provider: CSSwitch_Provider
    let number: Int
    let freePercent: Double

    /// The accounts that can run the configured model, each with the free
    /// share of its longest limit that covers every model.
    static func values(for accounts: [CSSwitch_Account]) -> [Self] {
        accounts.filter { account in
            guard account.offersConfiguredModel else { return false }
            if !account.menuProvider.isRouted {
                return account.state == .ready || account.state == .limited
            }
            return true
        }.compactMap { account in
            guard let allowance = account.allowance.windows
                .filter({ $0.modelID.isEmpty && $0.modelName.isEmpty })
                .max(by: { $0.periodSeconds < $1.periodSeconds }) else { return nil }
            return Self(provider: account.menuProvider, number: Int(account.accountNumber), freePercent: allowance.freePercent)
        }
    }
}

struct MenuBarProviderGroup: Equatable, Sendable {
    let provider: CSSwitch_Provider
    let accounts: [MenuBarAccountValue]

    var averageFreePercent: Double {
        guard !accounts.isEmpty else { return 0 }
        let sum = accounts.reduce(0.0) { $0 + min(max(0, $1.freePercent), 100) }
        return sum / Double(accounts.count)
    }

    var accountsRemaining: Double {
        accounts.reduce(0.0) { $0 + min(max(0, $1.freePercent), 100) / 100 }
    }

    static func groups(for accounts: [MenuBarAccountValue]) -> [Self] {
        var order: [CSSwitch_Provider] = []
        var byProvider: [CSSwitch_Provider: [MenuBarAccountValue]] = [:]
        for account in accounts {
            if byProvider[account.provider] == nil {
                order.append(account.provider)
            }
            byProvider[account.provider, default: []].append(account)
        }
        return order.map { Self(provider: $0, accounts: byProvider[$0] ?? []) }
    }
}
