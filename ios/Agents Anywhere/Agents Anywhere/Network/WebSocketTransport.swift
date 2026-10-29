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
                        // A lost final frame has no later sequence to reveal
                        // the gap. Reconnect so consumers recover their cursor.
                        if case .dropped = continuation.yield(data) {
                            continuation.finish(throwing: HTTPError.streamOverflow)
                            task.cancel(with: .goingAway, reason: nil)
                            return
                        }
                    }
                    continuation.finish()
                } catch {
                    continuation.finish(throwing: error)
                }
            }
            // A cellular NAT can drop an otherwise idle mapping well before the
            // server's 15 s keepalive frames matter, and a half-open socket
            // surfaces only as silence. A control ping every 20 s keeps the
            // mapping alive and fails fast when the path is gone.
            let keepalivePing = Task { [weak task] in
                while !Task.isCancelled {
                    do { try await Task.sleep(for: .seconds(20)) } catch { return }
                    guard let task, !Task.isCancelled else { return }
                    task.sendPing { error in
                        if error != nil {
                            // The socket is unwritable; end the stream so the
                            // owner reconnects instead of waiting for silence.
                            continuation.finish(throwing: URLError(.timedOut))
                            task.cancel(with: .goingAway, reason: nil)
                        }
                    }
                }
            }
            let watchdog = Task {
                while !Task.isCancelled {
                    try? await Task.sleep(for: .seconds(5))
                    if Task.isCancelled { return }
                    if lastFrame.silence > Self.silenceLimit {
                        // Finish with a timeout before cancelling: letting the
                        // pending receive surface URLError.cancelled made a
                        // single dead-silent socket look like a client
                        // cancellation, which is not retryable and downgraded
                        // the whole app to the legacy sockets for good.
                        continuation.finish(throwing: URLError(.timedOut))
                        task.cancel(with: .goingAway, reason: nil)
                        return
                    }
                }
            }
            continuation.onTermination = { [task] _ in
                receiveTask.cancel()
                watchdog.cancel()
                keepalivePing.cancel()
                task.cancel(with: .goingAway, reason: nil)
            }
        }
    }

    nonisolated func close() {
        task.cancel(with: .normalClosure, reason: nil)
    }
}

/// Frame activity shared by a receive task and its watchdogs.
///
/// These used to be local `var`s captured by two Tasks. That is an
/// unsynchronized data race: a torn read of the instant could declare a
/// healthy socket dead, or keep a dead one alive.
nonisolated final class WebSocketFrameClock: @unchecked Sendable {
    private let lock = NSLock()
    private var last = ContinuousClock.now
    private var sawFirstFrame = false

    func touch() {
        lock.lock(); last = .now; sawFirstFrame = true; lock.unlock()
    }

    var silence: Duration {
        lock.lock(); defer { lock.unlock() }
        return last.duration(to: .now)
    }

    var receivedFirstFrame: Bool {
        lock.lock(); defer { lock.unlock() }
        return sawFirstFrame
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
