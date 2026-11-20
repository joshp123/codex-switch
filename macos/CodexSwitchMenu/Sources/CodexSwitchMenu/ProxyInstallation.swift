import Foundation
import OSLog

private let installationLogger = Logger(
    subsystem: Bundle.main.bundleIdentifier ?? "CodexSwitchMenu",
    category: "ProxyInstallation"
)

/// The shell entry point is shared with source installs. Its child keeps the
/// handover running if Sparkle replaces the menu while a generation drains.
@MainActor
final class ProxyInstallation {
    typealias Runner = @MainActor (URL, [String: String], @escaping @Sendable (Int32) -> Void) throws -> AnyObject

    private enum State {
        case idle
        case running(AnyObject)
        case waitingToRetry(Task<Void, Never>)
    }

    private struct Configuration {
        let dataDirectory: URL
        let environment: [String: String]
    }

    private var state: State = .idle
    private var configuration: Configuration?
    private var lastRecoveryAttempt: ContinuousClock.Instant?
    private let retryDelay: Duration
    private let recoveryCooldown: Duration
    private let clock = ContinuousClock()
    private let runInstaller: Runner

    init(
        retryDelay: Duration = .seconds(60),
        recoveryCooldown: Duration = .seconds(60),
        runInstaller: Runner? = nil
    ) {
        self.retryDelay = retryDelay
        self.recoveryCooldown = recoveryCooldown
        self.runInstaller = runInstaller ?? Self.runBundledInstaller
    }

    func start(dataDirectory: URL, environment: [String: String]) {
        configuration = Configuration(dataDirectory: dataDirectory, environment: environment)
        switch state {
        case .running:
            return
        case let .waitingToRetry(task):
            task.cancel()
            state = .idle
            launch()
        case .idle:
            launch()
        }
    }

    /// Re-runs the installer if the proxy went unreachable after launch
    /// (for example, if launchd unloaded the job while the menu stayed open).
    func recoverIfNeeded() {
        guard configuration != nil, case .idle = state else { return }
        let now = clock.now
        if let lastRecoveryAttempt, lastRecoveryAttempt.duration(to: now) < recoveryCooldown {
            return
        }
        lastRecoveryAttempt = now
        launch()
    }

    /// Cancels any pending retry when the menu quits. A running installer child
    /// is intentionally left alone so a handover completes across relaunches.
    func stop() {
        if case let .waitingToRetry(task) = state {
            task.cancel()
            state = .idle
        }
    }

    private func launch() {
        guard let configuration else { return }
        do {
            let token = try runInstaller(configuration.dataDirectory, configuration.environment) { [weak self] status in
                Task { @MainActor [weak self] in
                    self?.handleExit(status: status)
                }
            }
            state = .running(token)
        } catch {
            installationLogger.error("Proxy installation failed: \(error.localizedDescription, privacy: .public)")
            scheduleRetry()
        }
    }

    private func handleExit(status: Int32) {
        guard case .running = state else { return }
        if status == 0 {
            state = .idle
        } else {
            installationLogger.error("Proxy installation exited with status \(status); see proxy-install.log")
            scheduleRetry()
        }
    }

    private func scheduleRetry() {
        if case let .waitingToRetry(existing) = state {
            existing.cancel()
        }
        let delay = retryDelay
        let task = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard let self, !Task.isCancelled, case .waitingToRetry = self.state else { return }
            self.state = .idle
            self.launch()
        }
        state = .waitingToRetry(task)
    }

    private static func runBundledInstaller(
        dataDirectory: URL,
        environment: [String: String],
        onExit: @escaping @Sendable (Int32) -> Void
    ) throws -> AnyObject {
        let resources = Bundle.main.resourceURL!
        let install = resources.appending(path: "deploy/install-proxy.sh")
        let proxy = Bundle.main.bundleURL.appending(path: "Contents/Helpers/codex-switch")
        try FileManager.default.createDirectory(
            at: dataDirectory, withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )
        // Append: an installer from before a relaunch may still be writing.
        let logDescriptor = open(dataDirectory.appending(path: "proxy-install.log").path, O_WRONLY | O_APPEND | O_CREAT, 0o600)
        guard logDescriptor >= 0 else { throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO) }
        let output = FileHandle(fileDescriptor: logDescriptor, closeOnDealloc: true)
        let child = Process()
        child.executableURL = URL(filePath: "/bin/sh")
        child.arguments = [install.path, proxy.path]
        child.environment = ProcessInfo.processInfo.environment.merging(environment) { _, configured in configured }
        child.environment?["CODEX_SWITCH_MENU_APP"] = Bundle.main.bundleURL.path
        child.standardInput = FileHandle.nullDevice
        child.standardOutput = output
        child.standardError = output
        child.terminationHandler = { process in
            onExit(process.terminationStatus)
        }
        try child.run()
        return child
    }
}
