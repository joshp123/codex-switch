import AppKit

/// Draws the menu bar item as one template image: for each provider, its logo
/// followed by a compact summary in the chosen display mode.
@MainActor
enum StatusStripRenderer {
    static let buttonHorizontalPadding: CGFloat = 8

    private static let canvasHeight: CGFloat = 22
    private static let markSize: CGFloat = 15
    private static let markSpacing: CGFloat = 5
    private static let providerSpacing: CGFloat = 12
    private static let barWidth: CGFloat = 4.5
    private static let barHeight: CGFloat = 13
    private static let barCornerRadius: CGFloat = 2
    private static let barSpacing: CGFloat = 2.5
    private static let valueFont = NSFont.monospacedDigitSystemFont(ofSize: 13, weight: .semibold)
    private static let denominatorFont = NSFont.monospacedDigitSystemFont(ofSize: 10.5, weight: .medium)

    struct Mark: Equatable {
        let provider: CSSwitch_Provider
        let origin: CGFloat
    }

    enum Segment: Equatable {
        case text(origin: CGFloat, primary: String, secondary: String?)
        case bars(origin: CGFloat, fractions: [Double])
    }

    struct Layout: Equatable {
        let marks: [Mark]
        let segments: [Segment]
        let values: [String]
        let imageSize: NSSize
    }

    static func layout(
        for accounts: [MenuBarAccountValue],
        mode: MenuBarDisplayMode = .pooledAverage,
        locale: Locale = .autoupdatingCurrent
    ) -> Layout {
        let groups = MenuBarProviderGroup.groups(for: accounts)
        var x: CGFloat = 0
        var marks: [Mark] = []
        var segments: [Segment] = []
        var labels: [String] = []

        for (index, group) in groups.enumerated() {
            if index > 0 { x += providerSpacing }
            marks.append(Mark(provider: group.provider, origin: x))
            x += markSize + markSpacing

            switch mode {
            case .pooledAverage:
                let text = "\(Int(group.averageFreePercent.rounded()))%"
                segments.append(.text(origin: x, primary: text, secondary: nil))
                labels.append(text)
                x += ceil((text as NSString).size(withAttributes: [.font: valueFont]).width)

            case .miniBars:
                let fractions = group.accounts.map { min(max($0.freePercent / 100, 0), 1) }
                segments.append(.bars(origin: x, fractions: fractions))
                labels.append("\(Int(group.averageFreePercent.rounded()))%")
                if !fractions.isEmpty {
                    x += CGFloat(fractions.count) * barWidth + CGFloat(fractions.count - 1) * barSpacing
                }

            case .accountsLeft:
                if group.accounts.count > 1 {
                    let primary = UsageFormatting.oneDecimal(group.accountsRemaining, locale: locale)
                    let secondary = "/\(group.accounts.count)"
                    segments.append(.text(origin: x, primary: primary, secondary: secondary))
                    labels.append(primary + secondary)
                    let primaryWidth = ceil((primary as NSString).size(withAttributes: [.font: valueFont]).width)
                    let secondaryWidth = ceil((secondary as NSString).size(withAttributes: [.font: denominatorFont]).width)
                    x += primaryWidth + 1 + secondaryWidth
                } else {
                    let text = "\(Int(group.averageFreePercent.rounded()))%"
                    segments.append(.text(origin: x, primary: text, secondary: nil))
                    labels.append(text)
                    x += ceil((text as NSString).size(withAttributes: [.font: valueFont]).width)
                }
            }
        }

        return Layout(
            marks: marks,
            segments: segments,
            values: labels,
            imageSize: NSSize(width: ceil(x), height: canvasHeight)
        )
    }

    static func image(
        for accounts: [MenuBarAccountValue],
        mode: MenuBarDisplayMode = .pooledAverage,
        locale: Locale = .autoupdatingCurrent
    ) -> NSImage? {
        guard !accounts.isEmpty else { return nil }
        let layout = layout(for: accounts, mode: mode, locale: locale)
        let image = NSImage(size: layout.imageSize, flipped: false) { bounds in
            for mark in layout.marks {
                mark.provider.mark?.draw(
                    in: NSRect(x: mark.origin, y: floor((bounds.height - markSize) / 2), width: markSize, height: markSize),
                    from: .zero, operation: .sourceOver, fraction: 1
                )
            }
            let valueHeight = valueFont.ascender - valueFont.descender
            let valueY = floor((bounds.height - valueHeight) / 2)
            let baselineY = valueY - valueFont.descender

            for segment in layout.segments {
                switch segment {
                case let .text(origin, primary, secondary):
                    let primaryAttributes: [NSAttributedString.Key: Any] = [
                        .font: valueFont,
                        .foregroundColor: NSColor.black,
                    ]
                    (primary as NSString).draw(at: NSPoint(x: origin, y: valueY), withAttributes: primaryAttributes)
                    if let secondary {
                        let primaryWidth = ceil((primary as NSString).size(withAttributes: primaryAttributes).width)
                        let secondaryAttributes: [NSAttributedString.Key: Any] = [
                            .font: denominatorFont,
                            .foregroundColor: NSColor.black.withAlphaComponent(0.55),
                        ]
                        let secondaryY = baselineY + denominatorFont.descender
                        (secondary as NSString).draw(
                            at: NSPoint(x: origin + primaryWidth + 1, y: secondaryY),
                            withAttributes: secondaryAttributes
                        )
                    }

                case let .bars(origin, fractions):
                    let barY = floor((bounds.height - barHeight) / 2)
                    for (index, fraction) in fractions.enumerated() {
                        let barX = origin + CGFloat(index) * (barWidth + barSpacing)
                        let trackRect = NSRect(x: barX, y: barY, width: barWidth, height: barHeight)
                        let trackPath = NSBezierPath(roundedRect: trackRect, xRadius: barCornerRadius, yRadius: barCornerRadius)
                        NSColor.black.withAlphaComponent(0.24).setFill()
                        trackPath.fill()

                        if fraction > 0 {
                            let fillHeight = min(barHeight, max(2.5, barHeight * fraction))
                            NSGraphicsContext.saveGraphicsState()
                            trackPath.addClip()
                            NSColor.black.setFill()
                            let fillRect = NSRect(x: barX, y: barY, width: barWidth, height: fillHeight)
                            NSBezierPath(roundedRect: fillRect, xRadius: barCornerRadius, yRadius: barCornerRadius).fill()
                            NSGraphicsContext.restoreGraphicsState()
                        }
                    }
                }
            }
            return true
        }
        image.isTemplate = true
        image.accessibilityDescription = accessibilityDescription(for: accounts)
        return image
    }

    static func identityImage() -> NSImage? {
        guard let mark = CSSwitch_Provider.chatgpt.mark else { return nil }
        mark.size = NSSize(width: markSize, height: markSize)
        mark.accessibilityDescription = "Codex Switch"
        return mark
    }

    static func accessibilityDescription(for accounts: [MenuBarAccountValue]) -> String {
        accounts.map { account in
            "\(account.provider.title) \(account.number), \(UsageFormatting.percent(account.freePercent)) left"
        }.joined(separator: "; ")
    }
}
