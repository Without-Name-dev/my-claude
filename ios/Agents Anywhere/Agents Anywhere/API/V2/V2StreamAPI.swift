import Foundation

/// One message from `GET /api/v2/stream/ws`. The server sends `stream.ready`
/// first, then the dashboard snapshot, then session envelopes, refreshed
/// snapshots and keepalives. Keepalives are consumed by the stream and never
/// reach a consumer.
nonisolated enum V2StreamFrame: Hashable {
    case ready(serverTime: String)
    case dashboard(V2DashboardSnapshot)
    case session(V2SessionEvent)
    case keepalive
}

/// The unified realtime socket. It shares the wire transport with the legacy
/// session and dashboard sockets but decodes every frame kind from the same
/// `type` field, so the coordinator owns exactly one connection per app.
nonisolated struct V2StreamAPI {
    let serverURL: URL
    let webSocketTransport: any WebSocketTransport
    let decoder: JSONDecoder
    let heartbeatTimeout: Duration
    let heartbeatCheckInterval: Duration
    let firstFrameTimeout: Duration

    init(
        serverURL: URL,
        webSocketTransport: any WebSocketTransport,
        decoder: JSONDecoder = JSONDecoder(),
        heartbeatTimeout: Duration = .seconds(45),
        heartbeatCheckInterval: Duration = .seconds(15),
        firstFrameTimeout: Duration = .seconds(5)
    ) {
        self.serverURL = serverURL.normalizedV2ServerURL()
        self.webSocketTransport = webSocketTransport
        self.decoder = decoder
        self.heartbeatTimeout = heartbeatTimeout
        self.heartbeatCheckInterval = heartbeatCheckInterval
        self.firstFrameTimeout = firstFrameTimeout
    }

    /// Opens the unified stream. The first frame must arrive within
    /// `firstFrameTimeout`; silence for `heartbeatTimeout` afterwards is fatal.
    func stream(ticket: String) throws -> AsyncThrowingStream<V2StreamFrame, Error> {
        let connection = webSocketTransport.connect(url: try webSocketURL(path: "/api/v2/stream/ws", ticket: ticket))
        return AsyncThrowingStream(bufferingPolicy: .bufferingOldest(2048)) { continuation in
            var lastFrameAt = ContinuousClock.now
            var receivedFirstFrame = false

            // The ready frame is what distinguishes "connected" from "connected
            // but silent", so its deadline is much shorter than a normal frame gap.
            let firstFrameWatchdog = Task {
                do {
                    try await Task.sleep(for: firstFrameTimeout)
                    guard !receivedFirstFrame else { return }
                    continuation.finish(throwing: URLError(.timedOut))
                    connection.close()
                } catch { /* Consumer cancellation stops the watchdog. */ }
            }

            let watchdog = Task {
                do {
                    while !Task.isCancelled {
                        try await Task.sleep(for: heartbeatCheckInterval)
                        if lastFrameAt.duration(to: .now) >= heartbeatTimeout {
                            continuation.finish(throwing: URLError(.timedOut))
                            connection.close()
                            return
                        }
                    }
                } catch { /* Consumer cancellation stops the watchdog. */ }
            }

            let task = Task {
                do {
                    for try await data in connection.messages() {
                        lastFrameAt = .now
                        receivedFirstFrame = true
                        let frame = try Self.decodeFrame(data, decoder: decoder)
                        // Keepalives refresh the watchdog but are not values.
                        if case .keepalive = frame { continue }
                        if case .dropped = continuation.yield(frame) {
                            throw HTTPError.streamOverflow
                        }
                    }
                    continuation.finish()
                } catch let error as DecodingError {
                    continuation.finish(throwing: HTTPError.decoding(message: String(localized: "实时更新：\(error.v2Description)")))
                } catch {
                    continuation.finish(throwing: error)
                }
            }

            continuation.onTermination = { _ in
                firstFrameWatchdog.cancel()
                watchdog.cancel()
                task.cancel()
                connection.close()
            }
        }
    }

    /// Reads the frame kind and then the matching body from the same bytes.
    /// A session envelope's `type` is its event type, so every unknown value
    /// falls through to `V2SessionEvent`.
    private static func decodeFrame(_ data: Data, decoder: JSONDecoder) throws -> V2StreamFrame {
        let type = try decoder.decode(V2StreamFrameKind.self, from: data).type
        switch type {
        case "keepalive":
            return .keepalive
        case "stream.ready":
            return .ready(serverTime: try decoder.decode(V2StreamServerTime.self, from: data).serverTime)
        case "dashboard.snapshot":
            return .dashboard(try decoder.decode(V2DashboardSnapshot.self, from: data))
        default:
            var event = try decoder.decode(V2SessionEvent.self, from: data)
            event.wireBytes = data.count
            return .session(event)
        }
    }

    private func webSocketURL(path: String, ticket: String) throws -> URL {
        guard var components = URLComponents(
            url: URL(string: path, relativeTo: serverURL)?.absoluteURL ?? serverURL,
            resolvingAgainstBaseURL: false
        ) else {
            throw HTTPError.invalidRequestURL(path: path)
        }
        components.scheme = components.scheme == "https" ? "wss" : "ws"
        components.queryItems = [URLQueryItem(name: "ticket", value: ticket)]
        guard let url = components.url else {
            throw HTTPError.invalidRequestURL(path: path)
        }
        return url
    }
}

nonisolated private struct V2StreamFrameKind: Decodable {
    let type: String
}

nonisolated private struct V2StreamServerTime: Decodable {
    let serverTime: String
}
