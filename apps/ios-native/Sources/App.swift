import SwiftUI
import WebKit

/// A dependency-free shell: one WKWebView pointed at the deployed web UI.
/// No Capacitor/Cordova frameworks means the IPA has no nested binaries for a
/// sideloading tool to re-sign, which is the usual cause of "signed but will
/// not install" with a free Apple ID.
@main
struct ClaudeRemoteApp: App {
    private let url = URL(string: "http://8.166.138.207/code/")!

    var body: some Scene {
        WindowGroup {
            WebView(url: url)
                .ignoresSafeArea()
                .background(Color(red: 0.937, green: 0.933, blue: 0.914))
        }
    }
}

struct WebView: UIViewRepresentable {
    let url: URL

    func makeUIView(context: Context) -> WKWebView {
        let configuration = WKWebViewConfiguration()
        configuration.allowsInlineMediaPlayback = true
        configuration.mediaTypesRequiringUserActionForPlayback = []

        let web = WKWebView(frame: .zero, configuration: configuration)
        web.allowsBackForwardNavigationGestures = true
        // The page owns safe-area insets and the 100dvh layout.
        web.scrollView.contentInsetAdjustmentBehavior = .never
        web.isOpaque = false
        web.backgroundColor = .clear
        web.load(URLRequest(url: url))
        return web
    }

    func updateUIView(_ uiView: WKWebView, context: Context) {}
}
