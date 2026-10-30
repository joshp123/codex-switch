import Combine
import Foundation
import Observation
import Sparkle

@MainActor
@Observable
final class UpdateController {
    private let controller: SPUStandardUpdaterController
    // Sparkle holds its delegate weakly.
    private let installation = InstallDownloadedUpdatesAtOnce()
    private var observation: AnyCancellable?
    private(set) var canCheckForUpdates = false

    init() {
        controller = SPUStandardUpdaterController(
            startingUpdater: true,
            updaterDelegate: installation,
            userDriverDelegate: nil
        )
        observation = controller.updater.publisher(for: \.canCheckForUpdates)
            .receive(on: RunLoop.main)
            .sink { [weak self] canCheck in
                self?.canCheckForUpdates = canCheck
            }
    }

    func checkForUpdates() {
        controller.checkForUpdates(nil)
    }
}

/// A menu app that never quits would otherwise wait for a quit to install, and
/// a Mac nobody looks at never gets one. Quitting the menu leaves the proxy
/// running; the reopened app hands the proxy over without dropping work.
@MainActor
private final class InstallDownloadedUpdatesAtOnce: NSObject, SPUUpdaterDelegate {
    func updater(
        _ updater: SPUUpdater,
        willInstallUpdateOnQuit item: SUAppcastItem,
        immediateInstallationBlock immediateInstallHandler: @escaping () -> Void
    ) -> Bool {
        immediateInstallHandler()
        return true
    }
}
