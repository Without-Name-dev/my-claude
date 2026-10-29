import Foundation

struct V2APIClient {
    let serverURL: URL
    let account: V2AccountAPI
    let connectors: V2ConnectorAPI
    let projects: V2ProjectAPI
    let modelProviders: V2ModelProviderAPI
    let sessions: V2SessionAPI
    let runtime: V2RuntimeAPI
    let attachments: V2AttachmentAPI
    let realtime: V2RealtimeAPI
    let stream: V2StreamAPI
    private let ownedURLSession: URLSession?

    init(
        serverURL: URL,
        tokenProvider: any AuthTokenProvider,
        urlSession: URLSession? = nil
    ) {
        let session = urlSession ?? URLSession(configuration: V2MobileNetworking.configuration())
        ownedURLSession = urlSession == nil ? session : nil
        self.serverURL = serverURL.normalizedV2ServerURL()
        let transport = URLSessionHTTPTransport(
            serverURL: serverURL,
            urlSession: session,
            tokenProvider: tokenProvider
        )
        account = V2AccountAPI(transport: transport)
        connectors = V2ConnectorAPI(transport: transport)
        projects = V2ProjectAPI(transport: transport)
        modelProviders = V2ModelProviderAPI(transport: transport)
        sessions = V2SessionAPI(transport: transport)
        runtime = V2RuntimeAPI(transport: transport)
        attachments = V2AttachmentAPI(transport: transport)
        let webSocketTransport = URLSessionWebSocketTransport(urlSession: session)
        realtime = V2RealtimeAPI(
            serverURL: serverURL,
            transport: transport,
            webSocketTransport: webSocketTransport
        )
        stream = V2StreamAPI(serverURL: serverURL, webSocketTransport: webSocketTransport)
    }

    func cancelOutstandingRequests() { ownedURLSession?.invalidateAndCancel() }
}
