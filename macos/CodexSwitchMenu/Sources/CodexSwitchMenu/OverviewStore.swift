import Foundation
import Observation
import OSLog

private let snapshotLogger = Logger(
    subsystem: Bundle.main.bundleIdentifier ?? "CodexSwitchMenu",
    category: "Snapshot"
)

/// Holds the latest menu snapshot. The last good snapshot is kept in the app's
/// caches directory so the menu renders at launch before the first fetch.
@MainActor
@Observable
final class OverviewStore {
    enum ConnectionState: Equatable, Sendable {
        case starting
        case connected(version: String, confirmedAt: Date?)
        case disconnected

        var isConnected: Bool {
            if case .connected = self { return true }
            return false
        }
    }

    private(set) var latestOverview: OverviewSnapshot?
    private(set) var connectionState: ConnectionState = .starting
    var latestSnapshot: CSSwitch_MenuSnapshot? { latestOverview?.menuSnapshot }
    /// When the proxy last confirmed the snapshot the menu shows.
    var lastUpdatedAt: Date? {
        guard let composedAt = latestSnapshot?.generatedAt.date else { return nil }
        if case let .connected(_, confirmedAt?) = connectionState {
            return max(composedAt, confirmedAt)
        }
        return composedAt
    }
    private(set) var isRefreshing = false
    var currentCheckSucceeded: Bool { connectionState.isConnected }

    private var activeRefreshTask: Task<Void, Never>?
    private var nextRequestSequence: UInt64 = 0
    private var appliedRequestSequence: UInt64 = 0
    private let client: any SnapshotLoading
    private let cacheFile: URL

    init(client: any SnapshotLoading, cacheFile: URL) {
        self.client = client
        self.cacheFile = cacheFile
        if let cached = try? MenuSnapshotDecoding.decode(Data(contentsOf: cacheFile)) {
            latestOverview = OverviewSnapshot(menuSnapshot: cached)
        }
    }

    /// Fetches the latest snapshot at once.
    func refresh() async {
        if let activeRefreshTask {
            await activeRefreshTask.value
            return
        }

        isRefreshing = true
        let refreshTask = Task { _ = await self.fetch(after: nil) }
        activeRefreshTask = refreshTask
        await refreshTask.value
        activeRefreshTask = nil
        isRefreshing = false
    }

    func cancelRefresh() {
        activeRefreshTask?.cancel()
    }

    /// Waits until the proxy has a newer snapshot, or confirms the one shown is
    /// current, and takes it. Returns false when the proxy could not be reached.
    func waitForChange() async -> Bool {
        let shownVersion: String? = if case let .connected(version, _) = connectionState, !version.isEmpty {
            version
        } else {
            nil
        }
        return await fetch(after: shownVersion)
    }

    private func fetch(after shownVersion: String?) async -> Bool {
        nextRequestSequence &+= 1
        let sequence = nextRequestSequence

        let data: Data
        let latestVersion: String
        let snapshot: CSSwitch_MenuSnapshot
        do {
            switch try await client.loadSnapshot(after: shownVersion) {
            case let .current(confirmedAt):
                guard sequence >= appliedRequestSequence else { return true }
                appliedRequestSequence = sequence
                connectionState = .connected(version: shownVersion ?? "", confirmedAt: confirmedAt)
                return true
            case let .changed(changedData, changedVersion):
                data = changedData
                latestVersion = changedVersion
            }
            snapshot = try MenuSnapshotDecoding.decode(data)
        } catch is CancellationError {
            return true
        } catch {
            guard sequence >= appliedRequestSequence else { return currentCheckSucceeded }
            appliedRequestSequence = sequence
            connectionState = .disconnected
            snapshotLogger.error("Menu snapshot fetch failed: \(String(describing: error), privacy: .public)")
            return false
        }

        guard sequence >= appliedRequestSequence else { return true }
        appliedRequestSequence = sequence
        connectionState = .connected(version: latestVersion, confirmedAt: nil)
        latestOverview = OverviewSnapshot(menuSnapshot: snapshot)
        do {
            try FileManager.default.createDirectory(
                at: cacheFile.deletingLastPathComponent(),
                withIntermediateDirectories: true
            )
            try data.write(to: cacheFile, options: .atomic)
        } catch {
            snapshotLogger.error("Menu snapshot cache write failed: \(error.localizedDescription, privacy: .public)")
        }
        return true
    }
}
