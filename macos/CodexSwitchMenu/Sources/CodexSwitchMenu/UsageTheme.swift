import AppKit
import SwiftUI

enum UsageTheme {
    /// Wider than OpenUsage's 360 so the bar, the thing a person scans, is the
    /// widest part of a row even with "100% left" and "Resets Wed 18:00" beside it.
    static let panelWidth: CGFloat = 500
    static let outerInset: CGFloat = 14
    static let cardRadius: CGFloat = 12
    static let traySurface = Color(nsColor: .textBackgroundColor)

    /// OpenUsage's type scale: one title size, one label size, one detail size.
    static let titleFont = Font.system(size: 14, weight: .semibold)
    static let labelFont = Font.system(size: 13, weight: .semibold)
    static let detailFont = Font.system(size: 12)
    static let detailEmphasisFont = Font.system(size: 12, weight: .medium)

    /// Allowance is capacity, not an account identity. Reserve blue for
    /// this scale and keep account identity in a distinct categorical palette.
    static let capacityColour = Color(nsColor: .systemBlue)
    /// An allowance on pace to empty before it resets.
    static let runOutColour = Color(nsColor: .systemRed)

    /// Each provider's accounts share a family, so the ring splits by
    /// provider at a glance while every account stays distinct: Codex in
    /// violets and magentas, Claude in its warm oranges and browns. Green is
    /// reserved for Working, blue for the allowance bar and red for low, so
    /// no account wears them or anything close.
    private static let codexColours: [Color] = [
        .purple,
        Color(red: 0.86, green: 0.30, blue: 0.68),
        Color(red: 0.52, green: 0.38, blue: 0.80),
        Color(red: 0.80, green: 0.56, blue: 0.90),
        Color(red: 0.45, green: 0.24, blue: 0.52),
        Color(red: 0.68, green: 0.48, blue: 0.70),
    ]
    private static let claudeColours: [Color] = [
        .orange,
        Color(red: 0.90, green: 0.68, blue: 0.20),
        Color(red: 0.84, green: 0.44, blue: 0.30),
        .brown,
        Color(red: 0.96, green: 0.58, blue: 0.48),
        Color(red: 0.62, green: 0.46, blue: 0.26),
    ]
    private static let geminiColours: [Color] = [
        Color(red: 0.08, green: 0.64, blue: 0.62),
        Color(red: 0.22, green: 0.74, blue: 0.70),
        Color(red: 0.05, green: 0.48, blue: 0.50),
        Color(red: 0.36, green: 0.80, blue: 0.74),
        Color(red: 0.14, green: 0.56, blue: 0.58),
        Color(red: 0.46, green: 0.72, blue: 0.74),
    ]

    static func accountColour(_ provider: CSSwitch_Provider, _ number: Int) -> Color {
        let family: [Color]
        switch provider {
        case .chatgpt, .unspecified: family = codexColours
        case .claude: family = claudeColours
        case .gemini: family = geminiColours
        case .UNRECOGNIZED: return .secondary
        }
        return family[(max(number, 1) - 1) % family.count]
    }
}

/// How much air the panel has. Regular is OpenUsage's rhythm: 13-point
/// labels, 12-point detail, text rows 26 to 28 points apart. Compact is
/// what the panel falls back to only when regular would not fit the screen,
/// such as six accounts on a 14-inch laptop; it keeps every word and number.
enum PanelDensity {
    case regular, compact

    /// Pick compact up front on laptop-height screens when the pool already has
    /// enough accounts or providers to overflow regular density, avoiding a
    /// visible resize jump right after the panel opens.
    static func initial(for snapshot: CSSwitch_MenuSnapshot?, maximumHeight: CGFloat) -> PanelDensity {
        guard let snapshot, maximumHeight < 1_050 else { return .regular }
        if snapshot.accounts.count >= 5 || snapshot.providers.count >= 3 {
            return .compact
        }
        return .regular
    }

    var cardInset: CGFloat { self == .regular ? 12 : 7 }
    var sectionGap: CGFloat { self == .regular ? 10 : 5 }
    /// Between the identity group and the first window block, and between blocks.
    var groupGap: CGFloat { self == .regular ? 18 : 7 }
    var blockTitleGap: CGFloat { self == .regular ? 8 : 3 }
    var headerGap: CGFloat { self == .regular ? 6 : 3 }
    var summaryGap: CGFloat { self == .regular ? 10 : 6 }
    var identityFont: Font { .system(size: self == .regular ? 12 : 11) }
    var identityGap: CGFloat { self == .regular ? 4 : 2 }
    var rowFont: Font { .system(size: self == .regular ? 12 : 11) }
    var rowEmphasisFont: Font { .system(size: self == .regular ? 12 : 11, weight: .medium) }
    var rowHeight: CGFloat { self == .regular ? 18 : 14 }
    var rowGap: CGFloat { self == .regular ? 9 : 3 }
    var barHeight: CGFloat { 5 }
    var legendGap: CGFloat { self == .regular ? 5 : 2 }
    var ringSize: CGFloat { self == .regular ? 96 : 78 }
    /// Grid columns sized to the widest real strings: a dot and one digit,
    /// "100% left", and "Resets Wed 18:00" (a flame only ever comes with the
    /// shorter "Limit in …").
    var columnGap: CGFloat { 6 }
    var numberWidth: CGFloat { 20 }
    var percentWidth: CGFloat { self == .regular ? 58 : 54 }
    var timeWidth: CGFloat { self == .regular ? 136 : 128 }
}

private struct PanelDensityKey: EnvironmentKey {
    static let defaultValue = PanelDensity.regular
}

extension EnvironmentValues {
    var panelDensity: PanelDensity {
        get { self[PanelDensityKey.self] }
        set { self[PanelDensityKey.self] = newValue }
    }
}

extension View {
    func cardSurface() -> some View {
        background {
            RoundedRectangle(cornerRadius: UsageTheme.cardRadius, style: .continuous)
                .fill(.fill.quaternary)
        }
    }
}

/// The account's colour dot, used wherever an account is named.
struct AccountMarker: View {
    let colour: Color
    var size: CGFloat = 8

    var body: some View {
        Circle()
            .fill(colour)
            .frame(width: size, height: size)
            .accessibilityHidden(true)
    }
}
