// Standalone coordinator checks with fake transport. Compile with Swift 6.2:
// swiftc -o /tmp/stream-check 'Agents Anywhere/Agents Anywhere/Business/V2StreamCoordinator.swift' Tests/V2StreamCoordinatorChecks.swift
// /tmp/stream-check
import Foundation

#if os(Linux)
extension String { init(localized value: String) { self = value } }
#endif

typealias V2SessionID = String
struct V2SessionMeta {}
struct V2DashboardSnapshot { let type: String; let sessions: [V2SessionMeta] }
struct V2NetworkStatus {
    enum Availability { case offline, online }
    let availability: Availability
}
struct V2SessionEvent {
    let sessionId: String
    let eventId: String
    let wireBytes: Int
    static func subscribedMarker(sessionId: String) -> Self {
        Self(sessionId: sessionId, eventId: "subscribed", wireBytes: 1)
    }
}
enum HTTPError: Error { case streamOverflow }
struct V2ClientFailure {
    enum Kind { case cancelled, transient }
    let kind: Kind = .transient
    let permitsAutomaticReconnect = true
    init(_ error: Error) {}
}
@MainActor final class TimelineTrace {
    static let shared = TimelineTrace()
    func record(_ kind: String, _ detail: String) {}
}
enum V2RealtimeScope { case dashboard }
struct Ticket { let ticket = "ticket" }
protocol V2RealtimeAPIProtocol { func ticket(clientId: String, scope: V2RealtimeScope) async throws -> Ticket }
struct FakeRealtime: V2RealtimeAPIProtocol {
    func ticket(clientId: String, scope: V2RealtimeScope) async throws -> Ticket { Ticket() }
}
enum V2StreamFrame {
    case keepalive, ready(serverTime: String), dashboard(V2DashboardSnapshot), session(V2SessionEvent)
}
@MainActor final class FakeSocket {
    var continuation: AsyncThrowingStream<V2StreamFrame, Error>.Continuation?
    func stream() -> AsyncThrowingStream<V2StreamFrame, Error> {
        let pair = AsyncThrowingStream<V2StreamFrame, Error>.makeStream()
        continuation = pair.continuation
        return pair.stream
    }
}
@MainActor struct V2StreamAPI {
    let socket: FakeSocket
    func stream(ticket: String) throws -> AsyncThrowingStream<V2StreamFrame, Error> { socket.stream() }
}

@main
struct V2StreamCoordinatorChecks {
    @MainActor static func waitForSocket(_ socket: FakeSocket) async throws {
        for _ in 0..<200 {
            if socket.continuation != nil { return }
            try await Task.sleep(for: .milliseconds(10))
        }
        fatalError("socket did not start")
    }

    @MainActor static func main() async throws {
        let socket = FakeSocket()
        let coordinator = V2StreamCoordinator(realtimeAPI: FakeRealtime(), streamAPI: V2StreamAPI(socket: socket))
        let firstSubscription = Task { try await coordinator.sessionUpdates(id: "first") }
        try await waitForSocket(socket)
        socket.continuation?.yield(.ready(serverTime: ""))
        let firstStream = try await firstSubscription.value
        var first = firstStream.makeAsyncIterator()
        let firstMarker = try await first.next()
        precondition(firstMarker?.eventId == "subscribed")

        coordinator.setActive(false)
        socket.continuation = nil
        let waitingStream = try await coordinator.sessionUpdates(id: "waiting")
        var gotMarker = false
        let waiting = Task { @MainActor in
            var iterator = waitingStream.makeAsyncIterator()
            let result = try await iterator.next()
            gotMarker = true
            return result
        }
        try await Task.sleep(for: .milliseconds(100))
        precondition(!gotMarker, "a disconnected socket must not claim a live subscription")
        coordinator.setActive(true)
        try await waitForSocket(socket)
        socket.continuation?.yield(.ready(serverTime: ""))
        let resumedMarker = try await waiting.value
        precondition(resumedMarker?.eventId == "subscribed")

        let slowStream = try await coordinator.sessionUpdates(id: "slow")
        for index in 0..<300 {
            socket.continuation?.yield(.session(V2SessionEvent(sessionId: "slow", eventId: "\(index)", wireBytes: 1)))
        }
        try await Task.sleep(for: .milliseconds(200))
        var slow = slowStream.makeAsyncIterator()
        do {
            while try await slow.next() != nil {}
            fatalError("overflow must terminate the stream so recovery can run")
        } catch HTTPError.streamOverflow {
            // Expected: the dropped frame cannot be silently ignored.
        }
        coordinator.invalidate()
        print("Realtime stream coordinator checks passed")
    }
}
