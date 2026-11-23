import SwiftUI

struct CodexSwitchPanel: View {
    let store: OverviewStore
    let maximumHeight: CGFloat
    let openGeneration: Int
    let updates: UpdateController?
    let onRefresh: (() -> Void)?
    @State private var selectedPeriod: OverviewSnapshot.ReportingPeriodKind
    @State private var contentHeight: CGFloat = 0
    @State private var heightAboveTasks: CGFloat = 0
    @AppStorage("privacyModeEnabled") private var privacyModeEnabled = false
    @AppStorage("usageMetric") private var selectedMetric: UsageMetric = .cost
    @AppStorage("topTasksExpanded") private var topTasksExpanded = false
    /// Regular air unless the whole pool would not fit the screen; decided
    /// once per opening, from the first layout.
    @State private var density = PanelDensity.regular

    private var topInset: CGFloat { density == .regular ? 12 : 8 }
    private var bottomInset: CGFloat { density == .regular ? 10 : 6 }

    init(
        store: OverviewStore,
        maximumHeight: CGFloat,
        openGeneration: Int = 0,
        initialPeriod: OverviewSnapshot.ReportingPeriodKind = .today,
        updates: UpdateController? = nil,
        onRefresh: (() -> Void)? = nil
    ) {
        self.store = store
        self.maximumHeight = maximumHeight
        self.openGeneration = openGeneration
        self.updates = updates
        self.onRefresh = onRefresh
        _selectedPeriod = State(initialValue: initialPeriod)
        _density = State(initialValue: PanelDensity.initial(for: store.latestSnapshot, maximumHeight: maximumHeight))
    }

    /// The open top tasks list takes the height the screen leaves after the
    /// pool, so the whole panel fits without scrolling.
    private var taskRows: Int {
        guard topTasksExpanded, heightAboveTasks > 0 else { return 0 }
        let room = maximumHeight - PanelSizing.footerHeight - topInset - bottomInset
            - heightAboveTasks - density.sectionGap
        return RoutedTasksSection.rows(fitting: room, density: density)
    }

    var body: some View {
        let snapshot = store.latestSnapshot
        let overview = store.latestOverview
        let selectedPresentation = overview.flatMap {
            UsagePresentation(snapshot: $0, selectedPeriod: selectedPeriod)
        }
        VStack(spacing: 0) {
            ScrollViewReader { scrollProxy in
                ScrollView {
                    VStack(alignment: .leading, spacing: density.sectionGap) {
                        if let snapshot {
                            VStack(alignment: .leading, spacing: density.sectionGap) {
                                PanelStatusHeader(
                                    snapshot: snapshot,
                                    connectionState: store.connectionState,
                                    privacyModeEnabled: $privacyModeEnabled
                                )
                                let pace = WeeklyPace.lines(snapshot)
                                if !pace.isEmpty {
                                    VStack(alignment: .leading, spacing: 2) {
                                        ForEach(pace, id: \.self) { line in
                                            Text(line)
                                                .font(UsageTheme.detailFont)
                                                .foregroundStyle(.secondary)
                                        }
                                    }
                                    .padding(.leading, 4)
                                    .padding(.top, density == .regular ? -2 : -4)
                                }
                                if let overview, let selectedPresentation {
                                    UsageSummaryCard(
                                        presentation: selectedPresentation,
                                        selectedPeriod: selectedPeriod,
                                        selectedMetric: selectedMetric,
                                        enabledPeriods: Set(OverviewSnapshot.ReportingPeriodKind.allCases.filter {
                                            overview.supports(selectedMetric, period: $0)
                                        }),
                                        enabledMetrics: Set(UsageMetric.allCases.filter {
                                            overview.supports($0, period: selectedPeriod)
                                        }),
                                        selectPeriod: { selectedPeriod = $0 },
                                        selectMetric: { selectedMetric = $0 }
                                    )
                                }
                                ForEach(snapshot.providers, id: \.self) { provider in
                                    ProviderSection(
                                        snapshot: snapshot,
                                        provider: provider,
                                        hideIdentity: privacyModeEnabled,
                                        now: snapshot.generatedAt.date
                                    )
                                }
                            }
                            .background {
                                GeometryReader { proxy in
                                    Color.clear.preference(
                                        key: PanelHeightAboveTasksKey.self,
                                        value: proxy.size.height
                                    )
                                }
                            }
                            RoutedTasksSection(
                                snapshot: snapshot,
                                period: overview?.reportingPeriod(selectedPeriod),
                                selectedPeriod: selectedPeriod,
                                selectedMetric: selectedMetric,
                                maximumRows: taskRows,
                                expanded: $topTasksExpanded
                            )
                        } else {
                            EmptyPanelState(connectionState: store.connectionState)
                        }
                    }
                    .padding(.horizontal, UsageTheme.outerInset)
                    .padding(.top, topInset)
                    .padding(.bottom, bottomInset)
                    .environment(\.panelDensity, density)
                    .background {
                        GeometryReader { proxy in
                            Color.clear.preference(
                                key: PanelContentHeightKey.self,
                                value: proxy.size.height
                            )
                        }
                    }
                    .id(PanelScrollAnchor.top)
                }
                .scrollIndicators(
                    PanelSizing.contentRequiresScrolling(
                        contentHeight: contentHeight,
                        maximumPanelHeight: maximumHeight
                    ) ? .automatic : .hidden
                )
                .scrollBounceBehavior(.basedOnSize)
                .frame(height: contentHeight > 0 ? PanelSizing.scrollHeight(
                    contentHeight: contentHeight,
                    maximumPanelHeight: maximumHeight
                ) : nil)
                .frame(maxHeight: max(maximumHeight - PanelSizing.footerHeight, 120))
                .onPreferenceChange(PanelContentHeightKey.self) { height in
                    guard store.latestSnapshot != nil || contentHeight == 0 else { return }
                    contentHeight = height
                    updateDensity()
                }
                .onPreferenceChange(PanelHeightAboveTasksKey.self) { height in
                    heightAboveTasks = height
                    updateDensity()
                }
                .onChange(of: selectedPeriod) { _, _ in
                    scrollProxy.scrollTo(PanelScrollAnchor.top, anchor: .top)
                }
                .onChange(of: selectedMetric) { _, _ in
                    scrollProxy.scrollTo(PanelScrollAnchor.top, anchor: .top)
                }
            }

            UsageFooter(store: store, updates: updates, onRefresh: onRefresh)
        }
        // At least the standard width; wider when a line needs it, so nothing wraps.
        .frame(minWidth: UsageTheme.panelWidth)
        .focusEffectDisabled()
        .background(UsageTheme.traySurface.ignoresSafeArea())
        .onChange(of: openGeneration) { _, _ in
            density = PanelDensity.initial(for: store.latestSnapshot, maximumHeight: maximumHeight)
        }
        .onChange(of: maximumHeight) { _, newHeight in
            density = PanelDensity.initial(for: store.latestSnapshot, maximumHeight: newHeight)
        }
        .onChange(of: store.latestSnapshot, initial: true) { _, snapshot in
            guard let snapshot else { return }
            let overview = OverviewSnapshot(menuSnapshot: snapshot)
            if !overview.supports(selectedMetric, period: selectedPeriod),
               overview.supports(.tokens, period: selectedPeriod) {
                selectedMetric = .tokens
            }
        }
    }

    /// Measured at regular density; fall back to compact only when the pool itself overflows.
    /// A point short of the screen counts as not fitting, so the panel never sits exactly on the limit.
    private func updateDensity() {
        guard density == .regular else { return }
        let limit = maximumHeight - PanelSizing.footerHeight - 1
        let hasTaskSection = store.latestSnapshot.map {
            RoutedTasksSection.hasRows(snapshot: $0, period: store.latestOverview?.reportingPeriod(selectedPeriod))
        } ?? false
        let taskHeaderHeight = hasTaskSection
            ? density.sectionGap + RoutedTasksSection.height(rows: 0, density: density)
            : 0
        let poolHeight = topTasksExpanded
            ? (heightAboveTasks > 0
                ? heightAboveTasks + topInset + bottomInset + taskHeaderHeight
                : 0)
            : contentHeight
        if poolHeight >= limit {
            density = .compact
        }
    }
}

enum PanelSizing {
    static let footerHeight: CGFloat = 36

    static func scrollHeight(contentHeight: CGFloat, maximumPanelHeight: CGFloat) -> CGFloat {
        let maximumContentHeight = max(maximumPanelHeight - footerHeight, 120)
        return min(max(contentHeight, 1), maximumContentHeight)
    }

    static func contentRequiresScrolling(
        contentHeight: CGFloat,
        maximumPanelHeight: CGFloat
    ) -> Bool {
        contentHeight > scrollHeight(
            contentHeight: contentHeight,
            maximumPanelHeight: maximumPanelHeight
        )
    }
}

private enum PanelScrollAnchor {
    static let top = "panel-top"
}

private struct PanelContentHeightKey: PreferenceKey {
    static let defaultValue: CGFloat = 0
    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) {
        let next = nextValue()
        if next > 0 { value = next }
    }
}

private struct PanelHeightAboveTasksKey: PreferenceKey {
    static let defaultValue: CGFloat = 0
    static func reduce(value: inout CGFloat, nextValue: () -> CGFloat) {
        let next = nextValue()
        if next > 0 { value = next }
    }
}

private struct EmptyPanelState: View {
    let connectionState: OverviewStore.ConnectionState

    var body: some View {
        switch connectionState {
        case .disconnected:
            HStack(spacing: 6) {
                Image(systemName: "xmark.circle.fill")
                    .foregroundStyle(.secondary)
                Text("Not connected")
            }
            .font(UsageTheme.detailEmphasisFont)
            .frame(maxWidth: .infinity, minHeight: 80)
        case .starting, .connected:
            ProgressView()
                .controlSize(.small)
                .frame(maxWidth: .infinity, minHeight: 80)
        }
    }
}

private struct PanelStatusHeader: View {
    let snapshot: CSSwitch_MenuSnapshot
    let connectionState: OverviewStore.ConnectionState
    @Binding var privacyModeEnabled: Bool

    private enum ProxyState: Equatable {
        case starting, working, limited(String), notConnected(String)

        var title: String {
            switch self {
            case .starting: "Connecting"
            case .working: "Working"
            case let .limited(who): "\(who) limited"
            case let .notConnected(who): who.isEmpty ? "Not connected" : "\(who) not connected"
            }
        }

        var symbol: String {
            switch self {
            case .starting: "circle.dotted"
            case .working: "checkmark.circle.fill"
            case .limited: "pause.circle.fill"
            case .notConnected: "xmark.circle.fill"
            }
        }
    }

    /// Keep the Codex connection check and name any provider whose accounts are limited.
    private var state: ProxyState {
        let providers = snapshot.providers.filter(\.isRouted)
        switch connectionState {
        case .starting:
            return .starting
        case .disconnected:
            return .notConnected("")
        case .connected:
            break
        }
        let disconnected = providers.filter { provider in
            provider == .chatgpt && !snapshot.connection.newTasksConfigured
        }
        if !disconnected.isEmpty {
            return .notConnected(disconnected.count == providers.count ? "" : disconnected.map(\.title).joined(separator: ", "))
        }
        let limited = providers.filter { provider in
            !snapshot.accounts.contains { $0.menuProvider == provider && $0.usable }
        }
        if limited.isEmpty { return .working }
        return .limited(limited.count == providers.count ? "All accounts" : limited.map(\.title).joined(separator: ", "))
    }

    private var heading: String {
        let count = snapshot.accounts.filter { $0.menuProvider.isRouted && $0.offersConfiguredModel }.count
        switch count {
        case 0: return snapshot.accounts.isEmpty ? "No accounts" : "Usage overview"
        case 1: return "Balancing 1 account"
        default: return "Balancing \(count) accounts"
        }
    }

    var body: some View {
        HStack(alignment: .center, spacing: 10) {
            Text(heading)
                .font(UsageTheme.titleFont)
            Spacer(minLength: 8)
            HStack(spacing: 5) {
                Image(systemName: state.symbol)
                    .foregroundStyle(state == .working ? Color.green : Color.secondary)
                Text(state.title)
            }
            .font(UsageTheme.detailEmphasisFont)
            Button {
                privacyModeEnabled.toggle()
            } label: {
                Image(systemName: privacyModeEnabled ? "eye.slash" : "eye")
                    .font(.system(size: 13, weight: .medium))
                    .foregroundStyle(.secondary)
                    .frame(width: 18, height: 18)
                    .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .help(privacyModeEnabled ? "Show account emails" : "Hide account emails")
            .accessibilityLabel(privacyModeEnabled ? "Show account emails" : "Hide account emails")
        }
        .padding(.leading, 4)
        .padding(.trailing, 2)
    }
}
