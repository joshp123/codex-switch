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
