import SwiftUI

/// Device-page model gateway: this desktop's default provider and the
/// revocable credential its Claude sessions use. Every action is explicit and
/// every outcome is reported as it happened — issuing a credential while the
/// device is offline is not the same as the device being wired.
struct DeviceModelGatewaySection: View {
    let connector: V2Connector
    @Environment(AppState.self) private var appState

    @State private var providers: [V2ModelProvider] = []
    @State private var route: V2ConnectorModelRoute?
    @State private var loadFailed = false
    @State private var busy: Action?
    @State private var applyAll = false
    @State private var message: String?

    private enum Action: Equatable { case defaultProvider, gateway }

    private var service: V2ModelProviderService? { appState.modelProviderService }
    private var gateway: V2GatewayStatus? { route?.gateway }
    private var credentialActive: Bool { gateway?.isActive == true }
    private var enabledProviders: [V2ModelProvider] { providers.filter(\.enabled) }

    var body: some View {
        DeviceSection(String(localized: "模型网关")) {
            if busy == .gateway {
                ProgressView().controlSize(.small)
            } else if credentialActive {
                DevicePillButton(title: String(localized: "停用网关")) { Task { await disableGateway() } }
                    .disabled(route == nil)
            } else {
                DevicePillButton(title: String(localized: "启用网关")) { Task { await enableGateway() } }
                    .disabled(route == nil)
            }
        } content: {
            VStack(alignment: .leading, spacing: 10) {
                defaultProviderRow
                gatewayStatusRow
                if let message {
                    Text(message).font(.caption).foregroundStyle(.secondary)
                } else if route == nil && !loadFailed {
                    Text(String(localized: "正在读取网关状态…")).font(.caption).foregroundStyle(.secondary)
                } else if loadFailed {
                    Button(String(localized: "重试")) { Task { await load() } }.font(.caption)
                }
            }
            .padding(.vertical, 4)
        }
        .task { await load() }
    }

    @ViewBuilder
    private var defaultProviderRow: some View {
        HStack(spacing: 10) {
            AppSymbol("network", size: 14).foregroundStyle(.secondary).frame(width: 18)
            Text(String(localized: "默认供应商")).font(.subheadline)
            Spacer(minLength: 8)
            if busy == .defaultProvider {
                ProgressView().controlSize(.small)
            } else if enabledProviders.isEmpty {
                Text(String(localized: "请先添加并启用供应商")).font(.caption).foregroundStyle(.secondary)
            } else {
                Menu {
                    Button(String(localized: "未设置")) { Task { await setDefault(nil) } }
                    ForEach(enabledProviders) { provider in
                        Button("\(provider.name) · \(provider.modelId)") {
                            Task { await setDefault(provider.id) }
                        }
                    }
                } label: {
                    Text(route?.defaultProvider?.providerName ?? String(localized: "未设置"))
                        .font(.subheadline).lineLimit(1)
                }
                .disabled(busy != nil)
            }
        }
        if route?.defaultProvider != nil {
            HStack(spacing: 10) {
                Color.clear.frame(width: 18, height: 1)
                Toggle(String(localized: "同时应用到这台电脑的所有会话"), isOn: $applyAll)
                    .font(.caption).tint(.green)
            }
        }
    }

    @ViewBuilder
    private var gatewayStatusRow: some View {
        HStack(spacing: 10) {
            AppSymbol("key", size: 14)
                .foregroundStyle(credentialActive ? Color.green : Color.secondary).frame(width: 18)
            VStack(alignment: .leading, spacing: 2) {
                Text(gatewayStatusTitle).font(.subheadline)
                if credentialActive {
                    Text(gateway?.connected == true
                         ? String(localized: "Claude 会话正在使用网关")
                         : String(localized: "已签发，但还没有请求使用过"))
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 8)
        }
    }

    private var gatewayStatusTitle: String {
        switch gateway?.credentialStatus {
        case "active": return String(localized: "网关凭证：已启用")
        case "revoked": return String(localized: "网关凭证：已撤销")
        default: return String(localized: "网关凭证：未签发")
        }
    }

    private func load() async {
        guard let service else {
            // Without services the section cannot show anything; say so
            // instead of leaving a permanent "loading" caption.
            loadFailed = true
            return
        }
        do {
            providers = try await service.providers()
            route = try await service.connectorRoute(connector.id)
            loadFailed = false
        } catch {
            loadFailed = route == nil
            message = route == nil
                ? nil
                : String(localized: "无法刷新网关状态，显示的是上次读取的结果。")
        }
    }

    private func setDefault(_ providerId: String?) async {
        guard let service, busy == nil else { return }
        busy = .defaultProvider
        message = nil
        defer { busy = nil }
        do {
            let response = try await service.setConnectorRoute(
                connector.id, providerId: providerId,
                applyToSessions: providerId != nil && applyAll ? "all" : "none")
            route = response.route
            // One pick should mean one action; leaving this on would silently
            // pin every session again the next time a provider is chosen.
            applyAll = false
            if let applied = response.applied {
                message = String(localized: "已应用 \(applied.pinned) 个会话，\(applied.failed) 个失败。")
            }
        } catch {
            message = error.localizedDescription
        }
    }

    private func enableGateway() async {
        guard let service, busy == nil else { return }
        busy = .gateway
        message = nil
        defer { busy = nil }
        do {
            let response = try await service.enableGateway(connector.id)
            route = V2ConnectorModelRoute(
                connectorId: connector.id,
                defaultProvider: route?.defaultProvider,
                gateway: response.gateway)
            message = response.pushed
                ? String(localized: "网关凭证已签发并推送到电脑。")
                : String(localized: "电脑不在线，设备上线后请重新启用。")
        } catch {
            message = error.localizedDescription
        }
    }

    private func disableGateway() async {
        guard let service, busy == nil else { return }
        busy = .gateway
        message = nil
        defer { busy = nil }
        do {
            let response = try await service.disableGateway(connector.id)
            route = V2ConnectorModelRoute(
                connectorId: connector.id,
                defaultProvider: route?.defaultProvider,
                gateway: response.gateway)
            message = response.pushed
                ? String(localized: "网关已停用。")
                : String(localized: "凭证已在服务器撤销；电脑当前离线，请让它重新连接以避免继续使用旧凭证。")
        } catch {
            message = error.localizedDescription
        }
    }
}
