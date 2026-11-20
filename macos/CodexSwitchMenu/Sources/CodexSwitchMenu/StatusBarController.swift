import AppKit
import SwiftUI

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
    private var resizeObserver: NSObjectProtocol?
    /// What the status item shows now; it is drawn again only when this changes.
    private var shownValues: [MenuBarAccountValue]?

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
        button.sendAction(on: [.leftMouseUp])
        updateStatusItem()

        // The proxy answers when its snapshot changes, so the menu does
        // nothing while nothing changes.
        refreshTask = Task { [weak self] in
            var consecutiveFailures = 0
            await self?.store.refresh()
            if let self, !self.store.currentCheckSucceeded {
                consecutiveFailures = 1
            }
            self?.updateStatusItem()
            while let self, !Task.isCancelled {
                if await self.store.waitForChange() {
                    consecutiveFailures = 0
                } else {
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
        closePanel()
    }

    @objc private func togglePanel() {
        guard statusItem.button != nil else { return }
        switch panelState {
        case .open:
            closePanel()
        case .closed:
            showPanel()
        }
    }

    /// Opens the panel directly under the status item, on that item's screen,
    /// kept inside the screen when the item sits near its edge.
    func showPanel() {
        guard let button = statusItem.button, let buttonWindow = button.window else { return }
        if case let .open(_, existingMonitors, existingRefreshTask) = panelState {
            for monitor in existingMonitors { NSEvent.removeMonitor(monitor) }
            existingRefreshTask.cancel()
        }
        let screen = buttonWindow.screen ?? NSScreen.main
        let visibleFrame = screen?.visibleFrame ?? NSRect(x: 0, y: 0, width: 1_512, height: 945)
        let buttonFrame = buttonWindow.convertToScreen(button.convert(button.bounds, to: nil))
        let top = visibleFrame.maxY - Self.screenMargin
        let maximumHeight = top - visibleFrame.minY - Self.screenMargin
        panelController.rootView = CodexSwitchPanel(store: store, maximumHeight: max(maximumHeight, 320), updates: updates)
        panelController.view.layoutSubtreeIfNeeded()
        let size = panelController.view.fittingSize
        // As wide as the content needs, never wider than the screen.
        let width = min(size.width, visibleFrame.width - 2 * Self.screenMargin)
        let x = min(max(buttonFrame.midX - width / 2, visibleFrame.minX + Self.screenMargin),
                    visibleFrame.maxX - width - Self.screenMargin)
        let anchorTopLeft = NSPoint(x: x, y: top)
        panel.setFrame(NSRect(x: anchorTopLeft.x, y: anchorTopLeft.y - size.height, width: width, height: size.height),
                       display: false)
        panel.makeKeyAndOrderFront(nil)
        let monitors = makeDismissMonitors()
        let openRefreshTask = Task { [weak self] in
            guard let self else { return }
            await self.store.refresh()
            self.updateStatusItem()
        }
        panelState = .open(anchorTopLeft: anchorTopLeft, eventMonitors: monitors, refreshTask: openRefreshTask)
    }

    private func closePanel() {
        if case let .open(_, eventMonitors, openRefreshTask) = panelState {
            for monitor in eventMonitors { NSEvent.removeMonitor(monitor) }
            openRefreshTask.cancel()
        }
        panelState = .closed
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
            let window = event.window
            let isOwnWindow = window == nil || window === self?.panel || window === self?.statusItem.button?.window
                || String(describing: type(of: window!)).contains("Menu")
            MainActor.assumeIsolated {
                if isEscape || (event.type != .keyDown && !isOwnWindow) {
                    self?.closePanel()
                }
            }
            return isEscape ? nil : event
        }) {
            monitors.append(local)
        }
        if let global = NSEvent.addGlobalMonitorForEvents(matching: clicks, handler: { [weak self] _ in
            Task { @MainActor [weak self] in self?.closePanel() }
        }) {
            monitors.append(global)
        }
        return monitors
    }

    private func updateStatusItem() {
        guard let button = statusItem.button else { return }
        let accounts = store.latestSnapshot?.accounts ?? []
        let values = MenuBarAccountValue.values(for: accounts)
        guard values != shownValues else { return }
        shownValues = values

        if values.isEmpty {
            showIdentity(button: button, toolTip: "Codex Switch")
            return
        }

        let toolTip = values.map { account in
            "\(account.provider.title) \(account.number): \(UsageFormatting.percent(account.freePercent)) left"
        }.joined(separator: "\n")
        guard let image = StatusStripRenderer.image(for: values) else {
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
