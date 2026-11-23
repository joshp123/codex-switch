import AppKit
import SwiftUI

struct UsageFooter: View {
    let store: OverviewStore
    var updates: UpdateController? = nil
    var onRefresh: (() -> Void)? = nil
    @AppStorage(MenuBarDisplayMode.storageKey) private var menuBarDisplayMode: MenuBarDisplayMode = .pooledAverage

    var body: some View {
        HStack {
            if let date = store.lastUpdatedAt {
                TimelineView(.periodic(from: .now, by: 10)) { context in
                    Text(updateLabel(date: date, now: context.date))
                }
                .foregroundStyle(.secondary)
            }
            Spacer()
            Menu("Options") {
                Button("Refresh now", systemImage: "arrow.clockwise") {
                    Task {
                        await store.refresh()
                        onRefresh?()
                    }
                }
                .disabled(store.isRefreshing)
                Menu("Menu bar") {
                    ForEach(MenuBarDisplayMode.allCases) { mode in
                        Button {
                            menuBarDisplayMode = mode
                            UserDefaults.standard.set(mode.rawValue, forKey: MenuBarDisplayMode.storageKey)
                            onRefresh?()
                        } label: {
                            if mode == menuBarDisplayMode {
                                Label(mode.title, systemImage: "checkmark")
                            } else {
                                Text(mode.title)
                            }
                        }
                    }
                }
                if let updates {
                    Button("Check for Updates…") {
                        updates.checkForUpdates()
                    }
                    .disabled(!updates.canCheckForUpdates)
                }
                Divider()
                Button("Quit", systemImage: "power") {
                    NSApplication.shared.terminate(nil)
                }
            }
            .menuStyle(.borderlessButton)
            .fixedSize()
        }
        .font(UsageTheme.detailFont)
        .padding(.leading, UsageTheme.outerInset + 4)
        .padding(.trailing, UsageTheme.outerInset)
        .padding(.vertical, 10)
        .background(UsageTheme.traySurface)
        .overlay(alignment: .top) { Divider() }
    }

    private func updateLabel(date: Date, now: Date) -> String {
        let seconds = max(Int(now.timeIntervalSince(date)), 0)
        switch seconds {
        case 0..<5: return "Updated now"
        case 5..<60: return "Updated \(seconds)s ago"
        case 60..<3_600: return "Updated \(seconds / 60)m ago"
        default: return "Updated \(seconds / 3_600)h ago"
        }
    }
}
