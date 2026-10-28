import Foundation

nonisolated protocol WebSocketConnection: AnyObject {
    func messages() -> AsyncThrowingStream<Data, Error>
    nonisolated func close()
}

nonisolated protocol WebSocketTransport {
    func connect(url: URL) -> any WebSocketConnection
}

nonisolated struct URLSessionWebSocketTransport: WebSocketTransport {
    let urlSession: URLSession

    init(urlSession: URLSession = .shared) {
        self.urlSession = urlSession
    }

    func connect(url: URL) -> any WebSocketConnection {
        let task = urlSession.webSocketTask(with: url)
        // URLSessionWebSocketTask refuses messages over 1 MiB by default and
        // fails the socket. A long streamed reply, a full timeline reset and a
        // dashboard snapshot all legitimately exceed that, so a single large
        // frame used to tear down the whole realtime stream.
        task.maximumMessageSize = 32 * 1024 * 1024
        return URLSessionWebSocketConnection(task: task)
    }
}

nonisolated final class URLSessionWebSocketConnection: WebSocketConnection, @unchecked Sendable {
    private let task: URLSessionWebSocketTask

    init(task: URLSessionWebSocketTask) {
        self.task = task
        task.resume()
    }

    /// How long the socket may stay silent before it is treated as dead.
    ///
    /// The server sends a keepalive every fifteen seconds. A network change —
    /// a router reboot, a handover — can leave a socket half-open: no error is
    /// raised, `receive()` simply returns nothing for ever, and the app looks
    /// connected while showing nothing. Three missed keepalives are enough to
    /// call it dead and let the owner reconnect.
    private static let silenceLimit: TimeInterval = 45

    /// Receives socket frames until the server closes or the consumer cancels.
    func messages() -> AsyncThrowingStream<Data, Error> {
        AsyncThrowingStream(bufferingPolicy: .bufferingOldest(2048)) { continuation in
            let lastFrame = LastFrameClock()
            let receiveTask = Task {
                do {
                    while !Task.isCancelled {
                        let message = try await task.receive()
                        lastFrame.touch()
                        let data: Data
                        switch message {
                        case let .data(bytes):
                            data = bytes
                        case let .string(text):
                            data = Data(text.utf8)
                        @unknown default:
                            continue
                        }
                        if case .dropped = continuation.yield(data) { throw HTTPError.streamOverflow }
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            let watchdog = Task {
                while !Task.isCancelled {
                    try? await Task.sleep(for: .seconds(5))
                    if Task.isCancelled { return }
                    if lastFrame.silence > Self.silenceLimit {
                        // Cancelling makes the pending receive throw, which
                        // finishes the stream with a real error for the caller.
                        task.cancel(with: .goingAway, reason: nil)
                        return
                    }
                }
            }
            continuation.onTermination = { [task] _ in
                receiveTask.cancel()
                watchdog.cancel()
                task.cancel(with: .goingAway, reason: nil)
            }
        }
    }

    nonisolated func close() {
        task.cancel(with: .normalClosure, reason: nil)
    }
}

/// The moment the last frame arrived, shared by the receive loop and its
/// watchdog.
private nonisolated final class LastFrameClock: @unchecked Sendable {
    private let lock = NSLock()
    private var last = Date()

    func touch() {
        lock.lock(); last = Date(); lock.unlock()
    }

    var silence: TimeInterval {
        lock.lock(); defer { lock.unlock() }
        return Date().timeIntervalSince(last)
    }
}
