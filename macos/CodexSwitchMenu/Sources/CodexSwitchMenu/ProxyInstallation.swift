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
    private var process: Process?
    private var retryTask: Task<Void, Never>?

    func start(dataDirectory: URL, environment: [String: String]) {
        guard process == nil else { return }
        let resources = Bundle.main.resourceURL!
        let install = resources.appending(path: "deploy/install-proxy.sh")
        let proxy = Bundle.main.bundleURL.appending(path: "Contents/Helpers/codex-switch")
        do {
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
            child.terminationHandler = { [weak self] child in
                let status = child.terminationStatus
                Task { @MainActor [weak self] in
                    self?.process = nil
                    if status != 0 {
                        installationLogger.error("Proxy installation exited with status \(status); see proxy-install.log")
                        self?.retry(dataDirectory: dataDirectory, environment: environment)
                    }
                }
            }
            try child.run()
            process = child
        } catch {
            installationLogger.error("Proxy installation failed: \(error.localizedDescription, privacy: .public)")
            retry(dataDirectory: dataDirectory, environment: environment)
        }
    }

    private func retry(dataDirectory: URL, environment: [String: String]) {
        retryTask = Task { [weak self] in
            try? await Task.sleep(for: .seconds(60))
            guard !Task.isCancelled else { return }
            self?.start(dataDirectory: dataDirectory, environment: environment)
        }
    }
}
