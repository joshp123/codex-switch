import AppKit
import SwiftUI

extension CSSwitch_Provider {
    var title: String {
        switch self {
        case .chatgpt, .unspecified: "Codex"
        case .claude: "Claude"
        case .gemini: "Gemini"
        case .UNRECOGNIZED: "Unknown"
        }
    }

    var isRouted: Bool {
        switch self {
        case .chatgpt, .claude, .unspecified: true
        case .gemini, .UNRECOGNIZED: false
        }
    }

    /// The provider's own logo, as a template image.
    var mark: NSImage? {
        let name: String? = switch self {
        case .chatgpt, .unspecified: "openai"
        case .claude: "claude"
        case .gemini: "gemini"
        case .UNRECOGNIZED: nil
        }
        guard let name else { return nil }
        let resources = Bundle.main.url(forResource: "CodexSwitchMenu_CodexSwitchMenu", withExtension: "bundle")
            .flatMap(Bundle.init(url:)) ?? .module
        guard let url = resources.url(forResource: name, withExtension: "svg", subdirectory: "Resources"),
              let image = NSImage(contentsOf: url) else { return nil }
        image.isTemplate = true
        return image
    }
}

extension CSSwitch_MenuSnapshot {
    /// The providers with accounts in the pool, in the order the proxy lists them.
    var providers: [CSSwitch_Provider] {
        accounts.reduce(into: []) { providers, account in
            if !providers.contains(account.menuProvider) { providers.append(account.menuProvider) }
        }
    }

    func account(key: String) -> CSSwitch_Account? {
        accounts.first { $0.key == key }
    }

    /// Each account keeps one colour everywhere, from its provider's family.
    func colour(of account: CSSwitch_Account) -> Color {
        UsageTheme.accountColour(account.menuProvider, Int(account.accountNumber))
    }
}

extension CSSwitch_Account {
    var menuProvider: CSSwitch_Provider { provider == .unspecified ? .chatgpt : provider }

    /// "Codex 1", "Claude 2": how an account is named outside its provider's section.
    var poolName: String { "\(menuProvider.title) \(accountNumber)" }
}

struct ProviderMark: View {
    let provider: CSSwitch_Provider
    var size: CGFloat = 16

    var body: some View {
        if let mark = provider.mark {
            Image(nsImage: mark)
                .renderingMode(.template)
                .resizable()
                .frame(width: size, height: size)
                .accessibilityLabel(provider.title)
        }
    }
}

/// The provider's logo and name above its accounts, as OpenUsage does.
struct ProviderHeader: View {
    let provider: CSSwitch_Provider

    var body: some View {
        HStack(spacing: 7) {
            ProviderMark(provider: provider, size: 18)
                .foregroundStyle(.secondary)
            Text(provider.title)
                .font(UsageTheme.titleFont)
            Spacer()
        }
    }
}
