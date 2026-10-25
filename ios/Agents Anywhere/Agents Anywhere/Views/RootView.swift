import SwiftUI

struct RootView: View {
    @Environment(AppState.self) private var appState
    @Environment(\.colorScheme) private var colorScheme
    @Environment(\.scenePhase) private var scenePhase
    @State private var showingManualLogin = false

    var body: some View {
        Group {
            switch appState.route {
            case .loading:
                Color(uiColor: .systemBackground)
            case .signedOut:
                ServiceEntryView(
                    onManualLogin: { showingManualLogin = true },
                )
            case .signedIn:
                ChatShellView()
            }
        }
        .sheet(isPresented: $showingManualLogin) {
            ManualLoginView(appState: appState) {
                appState.activateSignedInRoute()
                showingManualLogin = false
            }
        }
        .onChange(of: scenePhase) { _, phase in
            // The unified realtime socket must not stay open in the background;
            // foregrounding reconnects and asks every open session to recover.
            if phase == .background { appState.nativeChatServices?.streamCoordinator.setActive(false) }
            else if phase == .active { appState.nativeChatServices?.streamCoordinator.setActive(true) }
        }
        .announcementGate(
            server: appState.serverURL,
            route: appState.route,
            blocked: showingManualLogin
        )
        .overlay(alignment: .top) {
            if appState.route == .signedIn, let error = appState.restoreConnectionError {
                HStack(spacing: 12) {
                    AppSymbol("wifi.exclamationmark")
                    VStack(alignment: .leading, spacing: 2) {
                        Text(String(localized: "暂时无法连接，正在显示本地内容")).font(.footnote.weight(.medium))
                        Text(error).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                    }
                    Menu {
                        Button(String(localized: "立即重试"), action: retryServerConnection)
                        Button(String(localized: "返回登录"), action: appState.returnToLogin)
                    } label: { AppSymbol("ellipsis").frame(width: 36, height: 36) }
                }
                .padding(14).glassEffect(.regular, in: .rect(cornerRadius: 20))
                .padding(.horizontal, 22).padding(.top, 70).frame(maxWidth: 540)
            }
        }
        .tint(AppTheme.primaryText(colorScheme))
        .background(AppTheme.appBackground(colorScheme))
    }

    private func retryServerConnection() {
        Task {
            await appState.retryServerConnection()
        }
    }
}

#Preview {
    RootView()
        .environment(AppState())
}
