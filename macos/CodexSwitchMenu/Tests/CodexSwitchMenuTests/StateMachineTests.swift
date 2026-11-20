import Foundation
import SwiftProtobuf
import XCTest
@testable import CodexSwitchMenu

@MainActor
final class StateMachineTests: XCTestCase {
    func testStaleWaitFailureDoesNotClobberNewerRefresh() async throws {
        let first = try makeSnapshotBytes(generatedAt: Date(timeIntervalSince1970: 1_000))
        let second = try makeSnapshotBytes(generatedAt: Date(timeIntervalSince1970: 2_000))
        let client = ScriptedSnapshotClient()
        let cacheFile = FileManager.default.temporaryDirectory
            .appending(path: "\(UUID().uuidString)/menu-snapshot.pb")
        let store = OverviewStore(client: client, cacheFile: cacheFile)

        // Initial connect at v1.
        await client.enqueue(.success(.changed(first, version: "1.1")))
        await store.refresh()
        XCTAssertEqual(store.connectionState, .connected(version: "1.1", confirmedAt: nil))

        // Start a long-poll wait that hangs, then run a newer refresh that succeeds at v2.
        let waitGate = AsyncGate()
        await client.enqueueBlocked(gate: waitGate, result: .failure(SnapshotClientError.unreachable(.cannotConnectToHost)))
        let waitTask = Task { await store.waitForChange() }
        await waitGate.waitUntilEntered()

        await client.enqueue(.success(.changed(second, version: "2.1")))
        await store.refresh()
        XCTAssertEqual(store.connectionState, .connected(version: "2.1", confirmedAt: nil))
        XCTAssertEqual(store.latestSnapshot?.generatedAt.date, Date(timeIntervalSince1970: 2_000))

        // Now release the stale v1 long-poll failure: it must not mark the store disconnected.
        await waitGate.release()
        let waitResult = await waitTask.value
        XCTAssertTrue(waitResult)
        XCTAssertEqual(store.connectionState, .connected(version: "2.1", confirmedAt: nil))
        XCTAssertTrue(store.currentCheckSucceeded)
    }

    func testDisconnectClearsAfterVersionForNextWait() async throws {
        let first = try makeSnapshotBytes(generatedAt: Date(timeIntervalSince1970: 1_000))
        let second = try makeSnapshotBytes(generatedAt: Date(timeIntervalSince1970: 2_000))
        let client = ScriptedSnapshotClient()
        let cacheFile = FileManager.default.temporaryDirectory
            .appending(path: "\(UUID().uuidString)/menu-snapshot.pb")
        let store = OverviewStore(client: client, cacheFile: cacheFile)

        await client.enqueue(.success(.changed(first, version: "1.1")))
        await store.refresh()
        XCTAssertEqual(store.connectionState, .connected(version: "1.1", confirmedAt: nil))

        await client.enqueue(.failure(SnapshotClientError.unreachable(.cannotConnectToHost)))
        let failed = await store.waitForChange()
        XCTAssertFalse(failed)
        XCTAssertEqual(store.connectionState, .disconnected)

        // After disconnect, waitForChange must request with after == nil so reconnect answers immediately.
        await client.enqueue(.success(.changed(second, version: "2.1")))
        let recovered = await store.waitForChange()
        XCTAssertTrue(recovered)
        let versions = await client.recordedVersions()
        XCTAssertEqual(versions, [nil, "1.1", nil])
        XCTAssertEqual(store.connectionState, .connected(version: "2.1", confirmedAt: nil))
    }

    func testProxyInstallationRecoversAfterCleanExitAndRespectsCooldown() async throws {
        var launches = 0
        var exitHandlers: [@Sendable (Int32) -> Void] = []
        let installer = ProxyInstallation(
            retryDelay: .milliseconds(30),
            recoveryCooldown: .seconds(60)
        ) { _, _, onExit in
            launches += 1
            exitHandlers.append(onExit)
            return NSObject()
        }
        let dir = URL(filePath: "/tmp/codex-switch-test")

        installer.start(dataDirectory: dir, environment: [:])
        XCTAssertEqual(launches, 1)
        // Ignored while already running.
        installer.start(dataDirectory: dir, environment: [:])
        installer.recoverIfNeeded()
        XCTAssertEqual(launches, 1)

        // Finish cleanly -> idle.
        exitHandlers[0](0)
        try await Task.sleep(for: .milliseconds(20))

        // Unreachable proxy triggers recovery once, then cooldown blocks immediate repeat.
        installer.recoverIfNeeded()
        XCTAssertEqual(launches, 2)
        exitHandlers[1](0)
        try await Task.sleep(for: .milliseconds(20))

        installer.recoverIfNeeded()
        XCTAssertEqual(launches, 2)
    }

    func testProxyInstallationStopCancelsPendingRetry() async throws {
        var launches = 0
        var exitHandlers: [@Sendable (Int32) -> Void] = []
        let installer = ProxyInstallation(
            retryDelay: .milliseconds(40),
            recoveryCooldown: .zero
        ) { _, _, onExit in
            launches += 1
            exitHandlers.append(onExit)
            return NSObject()
        }
        let dir = URL(filePath: "/tmp/codex-switch-test")

        installer.start(dataDirectory: dir, environment: [:])
        XCTAssertEqual(launches, 1)
        exitHandlers[0](1)
        try await Task.sleep(for: .milliseconds(10))

        installer.stop()
        try await Task.sleep(for: .milliseconds(80))
        XCTAssertEqual(launches, 1)
    }

    private func makeSnapshotBytes(generatedAt: Date) throws -> Data {
        var snapshot = CSSwitch_MenuSnapshot()
        snapshot.generatedAt = Google_Protobuf_Timestamp(date: generatedAt)
        return try snapshot.serializedBytes()
    }
}

private actor AsyncGate {
    private var entered = false
    private var enteredWaiters: [CheckedContinuation<Void, Never>] = []
    private var released = false
    private var releaseWaiters: [CheckedContinuation<Void, Never>] = []

    func enterAndWait() async {
        entered = true
        for waiter in enteredWaiters { waiter.resume() }
        enteredWaiters.removeAll()
        if released { return }
        await withCheckedContinuation { continuation in
            releaseWaiters.append(continuation)
        }
    }

    func waitUntilEntered() async {
        if entered { return }
        await withCheckedContinuation { continuation in
            enteredWaiters.append(continuation)
        }
    }

    func release() {
        released = true
        for waiter in releaseWaiters { waiter.resume() }
        releaseWaiters.removeAll()
    }
}

private actor ScriptedSnapshotClient: SnapshotLoading {
    private enum Step {
        case immediate(Result<SnapshotFetch, Error>)
        case blocked(AsyncGate, Result<SnapshotFetch, Error>)
    }

    private var steps: [Step] = []
    private var versions: [String?] = []

    func enqueue(_ result: Result<SnapshotFetch, Error>) {
        steps.append(.immediate(result))
    }

    func enqueueBlocked(gate: AsyncGate, result: Result<SnapshotFetch, Error>) {
        steps.append(.blocked(gate, result))
    }

    func recordedVersions() -> [String?] {
        versions
    }

    func loadSnapshot(after version: String?) async throws -> SnapshotFetch {
        versions.append(version)
        guard !steps.isEmpty else { throw CancellationError() }
        switch steps.removeFirst() {
        case let .immediate(result):
            return try result.get()
        case let .blocked(gate, result):
            await gate.enterAndWait()
            return try result.get()
        }
    }
}
