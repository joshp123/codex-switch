import AppKit
import OSLog
import ServiceManagement
import SwiftUI

private let launchLogger = Logger(
    subsystem: Bundle.main.bundleIdentifier ?? "CodexSwitchMenu",
    category: "Launch"
)

@main
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    private var statusBarController: StatusBarController?
    private var updates: UpdateController?
    private let proxyInstallation = ProxyInstallation()
    private let launchConfiguration: LaunchConfiguration

    init(launchConfiguration: LaunchConfiguration) {
        self.launchConfiguration = launchConfiguration
    }

    static func main() {
        let launchConfiguration: LaunchConfiguration
        do {
            launchConfiguration = try LaunchConfiguration(arguments: Array(CommandLine.arguments.dropFirst()))
        } catch {
            launchLogger.error("Invalid launch arguments: \(String(describing: error), privacy: .public)")
            exit(2)
        }
        let application = NSApplication.shared
        let delegate = AppDelegate(launchConfiguration: launchConfiguration)
        application.delegate = delegate
        application.run()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        let cacheFile = launchConfiguration.installationEnvironment.isEmpty
            ? URL.cachesDirectory
                .appending(path: Bundle.main.bundleIdentifier ?? "CodexSwitchMenu", directoryHint: .isDirectory)
                .appending(path: "menu-snapshot.pb")
            : launchConfiguration.dataDirectory.appending(path: "menu-snapshot-cache.pb")
        let store = OverviewStore(
            client: SnapshotClient(dataDirectory: launchConfiguration.dataDirectory),
            cacheFile: cacheFile
        )
        NSApplication.shared.setActivationPolicy(.accessory)
        if Bundle.main.object(forInfoDictionaryKey: "SUFeedURL") != nil {
            updates = UpdateController()
        }
        let managesProxy = launchConfiguration.usesManagedDataDirectory &&
            Bundle.main.object(forInfoDictionaryKey: "CSManagesProxy") as? Bool == true
        statusBarController = StatusBarController(
            store: store,
            updates: updates,
            onProxyUnreachable: managesProxy ? { [weak self] in self?.proxyInstallation.recoverIfNeeded() } : nil
        )
        if managesProxy {
            proxyInstallation.start(
                dataDirectory: launchConfiguration.dataDirectory,
                environment: launchConfiguration.installationEnvironment
            )
        }
        if launchConfiguration.isProduction {
            registerLoginItem()
        }
    }

    /// Registers only an app that has never been registered (`.notFound` before
    /// the first registration), so a login item turned off in System Settings stays off.
    private func registerLoginItem() {
        let service = SMAppService.mainApp
        guard service.status == .notRegistered || service.status == .notFound else { return }
        do {
            try service.register()
        } catch {
            launchLogger.error("Login item registration failed: \(error.localizedDescription, privacy: .public)")
        }
    }

    func applicationShouldHandleReopen(
        _ sender: NSApplication,
        hasVisibleWindows flag: Bool
    ) -> Bool {
        statusBarController?.showPanel()
        return false
    }

    func applicationWillTerminate(_ notification: Notification) {
        proxyInstallation.stop()
        statusBarController?.stop()
    }
}
