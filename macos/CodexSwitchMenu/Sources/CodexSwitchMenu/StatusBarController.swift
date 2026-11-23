import AppKit
import OSLog
import SwiftUI

private let statusBarLogger = Logger(
    subsystem: Bundle.main.bundleIdentifier ?? "CodexSwitchMenu",
    category: "StatusBar"
)

/// What a closed panel shows: a store that never changes, so the panel's views
/// do no work while the real store takes new snapshots.
private struct ClosedPanelSnapshots: SnapshotLoading {
    func loadSnapshot(after version: String?) async throws -> SnapshotFetch { throw CancellationError() }
}

/// A borderless panel that can take key input without activating the app,
/// so Escape closes it and its menus open, while the app behind keeps focus.
private final class MenuBarPanel: NSPanel {
    override var canBecomeKey: Bool { true }
}

@MainActor
final class StatusBarController: NSObject {
    struct EventIdentity: Equatable {
        private static let mouseTypes: Set<NSEvent.EventType> = [
            .leftMouseDown, .leftMouseUp,
            .rightMouseDown, .rightMouseUp,
            .otherMouseDown, .otherMouseUp,
        ]

        let eventNumber: Int
        let timestamp: TimeInterval

        init?(_ event: NSEvent?) {
            guard let event, Self.mouseTypes.contains(event.type) else { return nil }
            self.eventNumber = event.eventNumber
            self.timestamp = event.timestamp
        }

        init(eventNumber: Int, timestamp: TimeInterval) {
            self.eventNumber = eventNumber
            self.timestamp = timestamp
        }

        func matches(_ other: EventIdentity) -> Bool {
            if eventNumber != 0, other.eventNumber != 0, eventNumber == other.eventNumber {
                return true
            }
            return abs(timestamp - other.timestamp) < 0.05
        }
    }

    struct ExternalDismiss: Equatable {
        let event: EventIdentity?
        let dismissedAt: ContinuousClock.Instant
    }

    private enum PanelState {
        case closed
        case open(anchorTopLeft: NSPoint, eventMonitors: [Any], refreshTask: Task<Void, Never>)
    }

    private let store: OverviewStore
    private let updates: UpdateController?
    private let onProxyUnreachable: (() -> Void)?
    private let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    private let panel: MenuBarPanel
    private let panelController: NSHostingController<CodexSwitchPanel>
    private let closedPanelStore = OverviewStore(client: ClosedPanelSnapshots(), cacheFile: URL(filePath: "/dev/null"))
    private var refreshTask: Task<Void, Never>?
    private var panelState: PanelState = .closed
    private var openGeneration: Int = 0
    private var openedByEvent: EventIdentity?
    private var dismissedByExternalClick: ExternalDismiss?
    private var resizeObserver: NSObjectProtocol?
    /// What the status item shows now; it is drawn again only when this changes.
    private var shownValues: [MenuBarAccountValue]?
    private var shownDisplayMode: MenuBarDisplayMode?

    /// Space between the menu bar and the panel, and between the panel and the screen's edges.
    private static let screenMargin: CGFloat = 6

    init(
        store: OverviewStore,
        updates: UpdateController? = nil,
        onProxyUnreachable: (() -> Void)? = nil
    ) {
        self.store = store
        self.updates = updates
        self.onProxyUnreachable = onProxyUnreachable
        panelController = NSHostingController(
            rootView: CodexSwitchPanel(
                store: closedPanelStore,
                maximumHeight: NSScreen.main?.visibleFrame.height ?? 700,
                updates: updates
            )
        )
        panelController.sizingOptions = [.preferredContentSize]
        // The hidden title bar still reserves its height as a safe area; the
        // panel's content starts at the window's top edge.
        panelController.safeAreaRegions = []
        panel = MenuBarPanel(
            contentRect: NSRect(x: 0, y: 0, width: UsageTheme.panelWidth, height: 400),
            styleMask: [.titled, .fullSizeContentView, .nonactivatingPanel],
            backing: .buffered,
            defer: true
        )
        super.init()

        // A titled window with its title bar hidden, not a borderless one with
        // a clear background: the window itself is opaque, with the system's
        // rounded corners and shadow, so everything that works from the
        // window's own shape (its shadow, the screenshot picker's highlight)
        // sees the whole panel rather than a clipped layer inside a clear frame.
        panel.contentViewController = panelController
        panel.titleVisibility = .hidden
        panel.titlebarAppearsTransparent = true
        panel.titlebarSeparatorStyle = .none
        for button in [NSWindow.ButtonType.closeButton, .miniaturizeButton, .zoomButton] {
            panel.standardWindowButton(button)?.isHidden = true
        }
        // Menu bar extras' own level: above every ordinary window, and treated
        // like one by the window picker of a screenshot.
        panel.level = .statusBar
        panel.hidesOnDeactivate = false
        panel.hasShadow = true
        panel.isMovable = false
        panel.isOpaque = true
        panel.backgroundColor = .textBackgroundColor
        panel.isExcludedFromWindowsMenu = true
        panel.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .ignoresCycle]
        panel.isReleasedWhenClosed = false
        resizeObserver = NotificationCenter.default.addObserver(
            forName: NSWindow.didResizeNotification, object: panel, queue: .main
        ) { [weak self] _ in
            MainActor.assumeIsolated { self?.keepAnchored() }
        }

        guard let button = statusItem.button else { return }
        button.target = self
        button.action = #selector(togglePanel)
        button.sendAction(on: [.leftMouseDown])
        updateStatusItem()

        // The proxy answers when its snapshot changes, so the menu does
        // nothing while nothing changes.
        refreshTask = Task { [weak self] in
            var consecutiveFailures = 0
            var rapidReturns = 0
            await self?.store.refresh()
            if let self, !self.store.currentCheckSucceeded {
                consecutiveFailures = 1
            }
            self?.updateStatusItem()
            while let self, !Task.isCancelled {
                let started = ContinuousClock.now
                if await self.store.waitForChange() {
                    consecutiveFailures = 0
                    if ContinuousClock.now - started < .seconds(1) {
                        rapidReturns += 1
                        if rapidReturns >= 2 {
                            try? await Task.sleep(for: .seconds(1))
                        }
                    } else {
                        rapidReturns = 0
                    }
                } else {
                    rapidReturns = 0
                    consecutiveFailures += 1
                    if consecutiveFailures >= 2 {
                        self.onProxyUnreachable?()
                    }
                    try? await Task.sleep(for: .seconds(5))
                }
                self.updateStatusItem()
            }
        }
    }

    func stop() {
        refreshTask?.cancel()
        refreshTask = nil
        if let resizeObserver {
            NotificationCenter.default.removeObserver(resizeObserver)
            self.resizeObserver = nil
        }
        store.cancelRefresh()
        closePanel(reason: "stop")
        NSStatusBar.system.removeStatusItem(statusItem)
    }

    @objc private func togglePanel() {
        guard statusItem.button != nil else { return }
        let currentEvent = EventIdentity(NSApp.currentEvent)
        switch panelState {
        case .open:
            statusBarLogger.debug("togglePanel open->close")
            closePanel(reason: "toggle", externalDismiss: nil)
        case .closed:
            if Self.shouldIgnoreToggleReopen(
                currentEvent: currentEvent,
                externalDismiss: dismissedByExternalClick,
                now: .now
            ) {
                statusBarLogger.debug("togglePanel suppressed reopen after external dismiss")
                dismissedByExternalClick = nil
                return
            }
            statusBarLogger.debug("togglePanel closed->open")
            showPanel()
        }
    }

    /// Opens the panel directly under the status item, on that item's screen,
    /// kept inside the screen when the item sits near its edge.
    func showPanel() {
        guard let button = statusItem.button,
              let buttonWindow = Self.anchorStatusBarWindow(
                  currentEventWindow: NSApp.currentEvent?.window,
                  buttonWindow: button.window
              ) else { return }
        if case let .open(_, existingMonitors, existingRefreshTask) = panelState {
            for monitor in existingMonitors { NSEvent.removeMonitor(monitor) }
            existingRefreshTask.cancel()
        }
        panelState = .closed
        dismissedByExternalClick = nil
        openGeneration &+= 1
        if let currentEvent = NSApp.currentEvent,
           [.leftMouseDown, .rightMouseDown, .otherMouseDown].contains(currentEvent.type) {
            openedByEvent = EventIdentity(currentEvent)
        } else {
            openedByEvent = nil
        }
        let screen = buttonWindow.screen ?? NSScreen.main
        let visibleFrame = screen?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1_512, height: 945)
        let buttonFrame = buttonWindow.frame.width > 0
            ? buttonWindow.frame
            : buttonWindow.convertToScreen(button.convert(button.bounds, to: nil))
        let top = visibleFrame.maxY - Self.screenMargin
        let maximumHeight = top - visibleFrame.minY - Self.screenMargin
        panelController.rootView = CodexSwitchPanel(
            store: store,
            maximumHeight: max(maximumHeight, 320),
            openGeneration: openGeneration,
            updates: updates,
            onRefresh: { [weak self] in self?.updateStatusItem() }
        )
        panelController.view.layoutSubtreeIfNeeded()
        let size = panelController.view.fittingSize
        // As wide as the content needs, never wider than the screen.
        let width = min(size.width, visibleFrame.width - 2 * Self.screenMargin)
        let x = min(max(buttonFrame.midX - width / 2, visibleFrame.minX + Self.screenMargin),
                    visibleFrame.maxX - width - Self.screenMargin)
        let anchorTopLeft = NSPoint(x: x, y: top)
        panel.setFrame(NSRect(x: anchorTopLeft.x, y: anchorTopLeft.y - size.height, width: width, height: size.height),
                       display: false)
        let monitors = makeDismissMonitors()
        let openRefreshTask = Task { [weak self] in
            guard let self else { return }
            await self.store.refresh()
            guard !Task.isCancelled else { return }
            self.updateStatusItem()
        }
        panelState = .open(anchorTopLeft: anchorTopLeft, eventMonitors: monitors, refreshTask: openRefreshTask)
        panel.makeKeyAndOrderFront(nil)
    }

    private func closePanel(reason: String = "manual", externalDismiss: ExternalDismiss? = nil) {
        if case let .open(_, eventMonitors, openRefreshTask) = panelState {
            for monitor in eventMonitors { NSEvent.removeMonitor(monitor) }
            openRefreshTask.cancel()
            statusBarLogger.debug("closePanel reason=\(reason, privacy: .public) gen=\(self.openGeneration)")
        }
        panelState = .closed
        openedByEvent = nil
        dismissedByExternalClick = externalDismiss
        panel.orderOut(nil)
        panelController.rootView = CodexSwitchPanel(store: closedPanelStore, maximumHeight: panelController.rootView.maximumHeight, updates: updates)
    }

    private func keepAnchored() {
        guard case let .open(anchorTopLeft, _, _) = panelState, panel.isVisible else { return }
        let frame = panel.frame
        let anchored = NSRect(x: anchorTopLeft.x, y: anchorTopLeft.y - frame.height, width: frame.width, height: frame.height)
        if anchored != frame { panel.setFrame(anchored, display: true) }
    }

    /// A click anywhere outside the panel, other than on the status item
    /// (whose own action toggles) or in one of the panel's own menus, or
    /// Escape, closes it.
    private func makeDismissMonitors() -> [Any] {
        var monitors: [Any] = []
        let clicks: NSEvent.EventTypeMask = [.leftMouseDown, .rightMouseDown, .otherMouseDown]
        if let local = NSEvent.addLocalMonitorForEvents(matching: clicks.union(.keyDown), handler: { [weak self] event in
            let isEscape = event.type == .keyDown && event.keyCode == 53
            let isKeyDown = event.type == .keyDown
            let window = event.window
            let incoming = EventIdentity(event)
            MainActor.assumeIsolated {
                guard let self else { return }
                if isEscape {
                    self.closePanel(reason: "escape", externalDismiss: nil)
                } else if !isKeyDown,
                          !Self.isOwnWindow(window, panel: self.panel, statusButtonWindow: self.statusItem.button?.window) {
                    self.closePanel(
                        reason: "local-other-window",
                        externalDismiss: ExternalDismiss(event: incoming, dismissedAt: .now)
                    )
                }
            }
            return isEscape ? nil : event
        }) {
            monitors.append(local)
        }
        let openingGeneration = openGeneration
        if let global = NSEvent.addGlobalMonitorForEvents(matching: clicks, handler: { [weak self] event in
            let location = NSEvent.mouseLocation
            let incoming = EventIdentity(event)
            if Thread.isMainThread {
                MainActor.assumeIsolated {
                    self?.handleGlobalClick(at: location, event: incoming, openingGeneration: openingGeneration)
                }
            } else {
                Task { @MainActor [weak self] in
                    self?.handleGlobalClick(at: location, event: incoming, openingGeneration: openingGeneration)
                }
            }
        }) {
            monitors.append(global)
        }
        return monitors
    }

    func handleGlobalClick(
        at location: NSPoint,
        event: EventIdentity?,
        openingGeneration: Int? = nil,
        now: ContinuousClock.Instant = .now
    ) {
        if let openingGeneration, openGeneration != openingGeneration { return }
        guard case .open = panelState else { return }
        if let openedByEvent, let event, openedByEvent.matches(event) { return }
        if Self.shouldDismissForOutsideClick(
            at: location,
            statusItemFrames: statusItemScreenFrames(),
            panelFrame: panel.frame
        ) {
            closePanel(
                reason: "global-outside-click",
                externalDismiss: ExternalDismiss(event: event, dismissedAt: now)
            )
        }
    }

    private func statusItemScreenFrames() -> [NSRect] {
        var frames: [NSRect] = []
        if let buttonWindow = statusItem.button?.window, buttonWindow.frame.width > 0, buttonWindow.frame.height > 0 {
            frames.append(buttonWindow.frame)
        }
        for window in NSApp.windows where Self.isStatusBarWindow(window) {
            let frame = window.frame
            if frame.width > 0, frame.height > 0, !frames.contains(frame) {
                frames.append(frame)
            }
        }
        return frames
    }

    static func isStatusBarWindow(_ window: NSWindow) -> Bool {
        String(describing: type(of: window)).contains("StatusBar")
    }

    static func anchorStatusBarWindow(currentEventWindow: NSWindow?, buttonWindow: NSWindow?) -> NSWindow? {
        if let currentEventWindow,
           isStatusBarWindow(currentEventWindow),
           currentEventWindow.frame.width > 0,
           currentEventWindow.frame.maxY > 0 {
            return currentEventWindow
        }
        return buttonWindow
    }

    /// macOS creates multiple NSStatusBarWindow instances per status item across
    /// displays and spaces; treat all of them, the panel, and popup menus as ours.
    static func isOwnWindow(
        _ window: NSWindow?,
        panel: NSWindow,
        statusButtonWindow: NSWindow?
    ) -> Bool {
        guard let window else { return true }
        if window === panel || window === statusButtonWindow { return true }
        let className = String(describing: type(of: window))
        return className.contains("Menu") || className.contains("StatusBar")
    }

    /// Global monitors also observe mouse-down events on a non-activating menu
    /// bar app's own status item (including its full window padding) or panel.
    /// Ignore clicks on any of them so the status item's toggle handles close once.
    static func shouldDismissForOutsideClick(
        at location: NSPoint,
        statusItemFrames: [NSRect],
        panelFrame: NSRect
    ) -> Bool {
        if panelFrame.contains(location) { return false }
        if statusItemFrames.contains(where: { $0.contains(location) }) { return false }
        return true
    }

    /// If a single click on the status item closed the panel via a dismiss
    /// monitor before reaching the status button's toggle action, do not reopen.
    static func shouldIgnoreToggleReopen(
        currentEvent: EventIdentity?,
        externalDismiss: ExternalDismiss?,
        now: ContinuousClock.Instant
    ) -> Bool {
        guard let externalDismiss else { return false }
        if let currentEvent, let dismissedEvent = externalDismiss.event, currentEvent.matches(dismissedEvent) {
            return true
        }
        return now - externalDismiss.dismissedAt < .milliseconds(250)
    }

    private func updateStatusItem() {
        guard let button = statusItem.button else { return }
        let accounts = store.latestSnapshot?.accounts ?? []
        let values = MenuBarAccountValue.values(for: accounts)
        let displayMode = MenuBarDisplayMode.current()
        guard values != shownValues || displayMode != shownDisplayMode else { return }
        shownValues = values
        shownDisplayMode = displayMode

        if values.isEmpty {
            showIdentity(button: button, toolTip: "Codex Switch")
            return
        }

        let toolTip = values.map { account in
            "\(account.provider.title) \(account.number): \(UsageFormatting.percent(account.freePercent)) left"
        }.joined(separator: "\n")
        guard let image = StatusStripRenderer.image(for: values, mode: displayMode) else {
            showIdentity(button: button, toolTip: toolTip)
            return
        }

        button.title = ""
        button.image = image
        button.imagePosition = .imageOnly
        statusItem.length = ceil(image.size.width) + StatusStripRenderer.buttonHorizontalPadding
        button.toolTip = toolTip
        button.setAccessibilityLabel(toolTip.replacingOccurrences(of: "\n", with: "; "))
    }

    private func showIdentity(button: NSStatusBarButton, toolTip: String) {
        if let image = StatusStripRenderer.identityImage() {
            statusItem.length = NSStatusItem.squareLength
            button.image = image
            button.title = ""
            button.imagePosition = .imageOnly
        } else {
            statusItem.length = NSStatusItem.variableLength
            button.image = nil
            button.title = "Codex Switch"
            button.imagePosition = .noImage
        }
        button.toolTip = toolTip
        button.setAccessibilityLabel(toolTip.replacingOccurrences(of: "\n", with: "; "))
    }
}
