import Darwin
import Foundation
import SwiftProtobuf

/// What the proxy said about its menu snapshot.
enum SnapshotFetch: Sendable {
    /// A snapshot newer than the one the menu shows, as protobuf bytes, and its version.
    case changed(Data, version: String)
    /// The snapshot the menu shows is still current, as of this time if the proxy reported one.
    case current(confirmedAt: Date?)
}

protocol SnapshotLoading: Sendable {
    /// The proxy's menu snapshot. Given the version the menu shows, the proxy
    /// answers as soon as a newer one exists, or within a minute to confirm
    /// the menu's is current; without one it answers at once.
    func loadSnapshot(after version: String?) async throws -> SnapshotFetch
}

enum SnapshotClientError: Error, Equatable {
    case unreadableURLFile
    case unreachable(URLError.Code)
    case status(Int)
    case invalidSnapshot
}

enum LaunchConfigurationError: Error, Equatable {
    case unexpectedArguments
    case relativeDataDirectory
    case invalidInstallationConfiguration
}

/// Bundled installation settings survive Sparkle relaunches. `--data-dir`
/// selects an existing proxy; only the production installation opens at login.
struct LaunchConfiguration: Equatable, Sendable {
    let dataDirectory: URL
    let isProduction: Bool
    let usesManagedDataDirectory: Bool
    let installationEnvironment: [String: String]

    init(arguments: [String]) throws {
        let productionDataDirectory = URL.applicationSupportDirectory.appending(path: "codex-switch", directoryHint: .isDirectory)
        let root = Bundle.main.object(forInfoDictionaryKey: "CSInstallationRoot") as? String
        let label = Bundle.main.object(forInfoDictionaryKey: "CSLaunchdLabel") as? String
        let listen = Bundle.main.object(forInfoDictionaryKey: "CSListenAddress") as? String
        let managedDataDirectory: URL
        if let root, let label, let listen {
            guard root.hasPrefix("/"), !label.isEmpty, !listen.isEmpty else {
                throw LaunchConfigurationError.invalidInstallationConfiguration
            }
            managedDataDirectory = URL(filePath: root, directoryHint: .isDirectory)
                .appending(path: "Library/Application Support/codex-switch", directoryHint: .isDirectory)
            installationEnvironment = [
                "HOME": root,
                "CODEX_SWITCH_LAUNCHD_LABEL": label,
                "CODEX_SWITCH_LISTEN": listen,
            ]
        } else {
            guard root == nil, label == nil, listen == nil else {
                throw LaunchConfigurationError.invalidInstallationConfiguration
            }
            managedDataDirectory = productionDataDirectory
            installationEnvironment = [:]
        }
        let filtered = arguments.filter { !$0.hasPrefix("-psn_") }
        if filtered.isEmpty {
            dataDirectory = managedDataDirectory
        } else {
            guard filtered.count == 2, filtered[0] == "--data-dir" else {
                throw LaunchConfigurationError.unexpectedArguments
            }
            guard filtered[1].hasPrefix("/") else { throw LaunchConfigurationError.relativeDataDirectory }
            dataDirectory = URL(filePath: filtered[1], directoryHint: .isDirectory)
        }
        let readsManagedDataDirectory = dataDirectory.standardizedFileURL == managedDataDirectory.standardizedFileURL
        usesManagedDataDirectory = readsManagedDataDirectory
        isProduction = root == nil && readsManagedDataDirectory
    }
}

/// Fetches the snapshot the proxy composes and serves at the URL it writes to
/// `menu-snapshot-url`. The file is read on every fetch because the port
/// changes when the proxy is redeployed; a stopping proxy answers its waiting
/// readers at once, so the next fetch reaches the new one.
struct SnapshotClient: SnapshotLoading {
    let urlFile: URL

    init(dataDirectory: URL) {
        urlFile = dataDirectory.appending(path: "menu-snapshot-url")
    }

    /// The proxy holds a waiting request for up to 55 seconds.
    private static let waitingTimeout: TimeInterval = 75
    private static let immediateTimeout: TimeInterval = 5

    private final class NoRedirectDelegate: NSObject, URLSessionTaskDelegate, Sendable {
        func urlSession(
            _ session: URLSession,
            task: URLSessionTask,
            willPerformHTTPRedirection response: HTTPURLResponse,
            newRequest request: URLRequest,
            completionHandler: @escaping @Sendable (URLRequest?) -> Void
        ) {
            completionHandler(nil)
        }
    }

    private static let noRedirectDelegate = NoRedirectDelegate()

    private static let session: URLSession = {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = waitingTimeout
        configuration.timeoutIntervalForResource = waitingTimeout
        configuration.urlCache = nil
        configuration.connectionProxyDictionary = [:]
        return URLSession(configuration: configuration, delegate: noRedirectDelegate, delegateQueue: nil)
    }()

    func loadSnapshot(after version: String?) async throws -> SnapshotFetch {
        var components = URLComponents(url: try snapshotURL(), resolvingAgainstBaseURL: false)
        if let version {
            components?.queryItems = [URLQueryItem(name: "after", value: version)]
        }
        guard let url = components?.url else { throw SnapshotClientError.unreadableURLFile }
        var request = URLRequest(url: url)
        request.timeoutInterval = version == nil ? Self.immediateTimeout : Self.waitingTimeout
        let data: Data
        let response: URLResponse
        do {
            (data, response) = try await Self.session.data(for: request)
        } catch let error as URLError {
            if error.code == .cancelled { throw CancellationError() }
            throw SnapshotClientError.unreachable(error.code)
        }
        let http = response as? HTTPURLResponse
        let latestVersion = http?.value(forHTTPHeaderField: "X-Snapshot-Version") ?? ""
        switch http?.statusCode ?? 0 {
        case 200:
            return .changed(data, version: latestVersion)
        case 304:
            let confirmed = http?.value(forHTTPHeaderField: "X-Snapshot-Composed-At")
                .flatMap(Self.parseComposedAt)
            return .current(confirmedAt: confirmed)
        case let status:
            throw SnapshotClientError.status(status)
        }
    }

    static func parseComposedAt(_ raw: String) -> Date? {
        if let date = try? Date.ISO8601FormatStyle(includingFractionalSeconds: true).parse(raw) {
            return date
        }
        return try? Date.ISO8601FormatStyle().parse(raw)
    }

    func snapshotURL() throws -> URL {
        guard let contents = try? String(contentsOf: urlFile, encoding: .utf8),
              let url = URL(string: contents.trimmingCharacters(in: .whitespacesAndNewlines)),
              url.scheme == "http",
              url.port != nil,
              let host = url.host?.lowercased(),
              Self.isLoopbackHost(host) else {
            throw SnapshotClientError.unreadableURLFile
        }
        return url
    }

    private static func isLoopbackHost(_ host: String) -> Bool {
        if host == "localhost" {
            return true
        }
        let stripped = (host.hasPrefix("[") && host.hasSuffix("]"))
            ? String(host.dropFirst().dropLast())
            : host
        var ipv4 = in_addr()
        if inet_pton(AF_INET, stripped, &ipv4) == 1 {
            return (UInt32(bigEndian: ipv4.s_addr) >> 24) == 127
        }
        var ipv6 = in6_addr()
        if inet_pton(AF_INET6, stripped, &ipv6) == 1 {
            return withUnsafeBytes(of: &ipv6) { bytes in
                bytes.prefix(15).allSatisfy { $0 == 0 } && bytes.last == 1
            }
        }
        return false
    }
}

enum MenuSnapshotDecoding {
    /// Decodes a snapshot and rejects one whose facts the panel cannot render.
    static func decode(_ data: Data) throws -> CSSwitch_MenuSnapshot {
        guard let snapshot = try? CSSwitch_MenuSnapshot(serializedBytes: data),
              snapshot.hasGeneratedAt,
              snapshot.accounts.allSatisfy(isValid) else {
            throw SnapshotClientError.invalidSnapshot
        }
        return snapshot
    }

    private static func isValid(_ account: CSSwitch_Account) -> Bool {
        guard !account.key.isEmpty, account.accountNumber > 0 else { return false }
        guard account.hasAllowance else { return true }
        return account.allowance.hasObservedAt && account.allowance.windows.allSatisfy { window in
            window.periodSeconds > 0 && window.usedPercent.isFinite &&
                (0...100).contains(window.usedPercent) && window.hasResetAt
        }
    }
}
