import Foundation

/// Numbers and dates follow the Mac's Language & Region settings, including a
/// custom number format. `NumberFormatter` and `DateFormatter` honour those
/// overrides; Foundation's `FormatStyle` ignores the custom number format.
enum UsageFormatting {
    static func privateEmail(_ value: String) -> String {
        let parts = value.split(separator: "@", maxSplits: 1, omittingEmptySubsequences: false)
        guard parts.count == 2,
              let first = parts[0].first,
              !parts[1].isEmpty else {
            return value
        }

        let domainParts = parts[1].split(separator: ".", omittingEmptySubsequences: false)
        let suffix = domainParts.count > 1 ? ".\(domainParts.last!)" : ""
        return "\(first)•••••@•••••\(suffix)"
    }

    /// When the window refills: "Resets Wed 21, 18:00".
    static func resetLabel(_ date: Date, now: Date, locale: Locale = .autoupdatingCurrent, timeZone: TimeZone = .autoupdatingCurrent) -> String {
        "Resets " + dayAndClock(date, locale: locale, timeZone: timeZone)
    }

    /// "Wed 21, 18:00", in the Mac's weekday, day and clock format. Resets
    /// are at most a month away, so the day of the month is never ambiguous.
    static func dayAndClock(_ date: Date, locale: Locale = .autoupdatingCurrent, timeZone: TimeZone = .autoupdatingCurrent) -> String {
        let formatter = DateFormatter()
        formatter.locale = locale
        formatter.timeZone = timeZone
        formatter.setLocalizedDateFormatFromTemplate("EEEdjmm")
        return formatter.string(from: date)
    }

    /// "2.6", "1.0": one decimal, as the Mac formats it.
    static func oneDecimal(_ value: Double, locale: Locale = .autoupdatingCurrent) -> String {
        decimal(value, fractionDigits: 1, locale: locale)
    }

    static func percent(_ value: Double, locale: Locale = .autoupdatingCurrent) -> String {
        decimal(value.rounded(), fractionDigits: 0, locale: locale) + "%"
    }

    /// Token counts to three significant figures: 946K, 34.1M, 6.88B.
    static func tokens(_ value: Int64, locale: Locale = .autoupdatingCurrent) -> String {
        let suffixes = ["", "K", "M", "B", "T"]
        var scaled = abs(Double(value))
        var power = 0
        while power < suffixes.count - 1, scaled >= 999.5 {
            scaled /= 1_000
            power += 1
        }
        let fractionDigits = switch scaled {
        case _ where power == 0: 0
        case ..<9.995: 2
        case ..<99.95: 1
        default: 0
        }
        return decimal(scaled, fractionDigits: fractionDigits, locale: locale) + suffixes[power]
    }

    /// API-equivalent cost in US dollars, about three significant figures:
    /// $4.99, $45.1, $648, $1,175. OpenAI prices in dollars, so the symbol is
    /// fixed while the digits follow the Mac.
    static func cost(_ nanoUSD: Int64, locale: Locale = .autoupdatingCurrent) -> String {
        let dollars = abs(Double(nanoUSD)) / 1_000_000_000
        let fractionDigits = switch dollars {
        case 0: 0
        case ..<9.995: 2
        case ..<99.95: 1
        default: 0
        }
        return "$" + decimal(dollars, fractionDigits: fractionDigits, locale: locale)
    }

    /// The cost inside the ring: as `cost` up to $9,999, then three
    /// significant figures, $10.6K, so it fits the ring as OpenUsage's does.
    static func ringCost(_ nanoUSD: Int64, locale: Locale = .autoupdatingCurrent) -> String {
        let dollars = abs(nanoUSD) / 1_000_000_000
        return dollars < 10_000 ? cost(nanoUSD, locale: locale) : "$" + tokens(dollars, locale: locale)
    }

    private static func decimal(_ value: Double, fractionDigits: Int, locale: Locale) -> String {
        let formatter = NumberFormatter()
        formatter.locale = locale
        formatter.numberStyle = .decimal
        formatter.usesGroupingSeparator = true
        formatter.minimumFractionDigits = fractionDigits
        formatter.maximumFractionDigits = fractionDigits
        return formatter.string(from: NSNumber(value: value)) ?? String(value)
    }
}
