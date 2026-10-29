import SwiftUI

/// Account-scoped model providers used by the model gateway. Credentials are
/// write-only: the list shows whether one is configured, never the value.
struct ModelProviderSettingsView: View {
    @Environment(AppState.self) private var appState
    @State private var providers: [V2ModelProvider]?
    @State private var loadError: String?
    @State private var editorTarget: EditorTarget?
    @State private var removeTarget: V2ModelProvider?
    /// Set only after the server reports that a provider is in use, so the
    /// force-delete confirmation is a second, distinct dialog. Reusing the
    /// first dialog's binding lost the 409 because its setter cleared the
    /// target before the response arrived.
    @State private var forceDeleteTarget: V2ModelProvider?
    @State private var removeReferences: ReferenceCounts?
    @State private var busyProviderID: String?
    @State private var checkMessage: String?
    @State private var alertTitle = String(localized: "检查结果")

    private struct EditorTarget: Identifiable {
        let id: String
        let provider: V2ModelProvider?
    }

    private struct ReferenceCounts {
        let connectors: Int
        let sessions: Int
    }

    var body: some View {
        List {
            if let providers {
                if providers.isEmpty {
                    Section {
                        ContentUnavailableView {
                            Label(String(localized: "还没有模型供应商"), appSymbol: "sparkles")
                        } description: {
                            Text(String(localized: "添加一个供应商，把这个账号的 Claude 会话路由到它。"))
                        } actions: {
                            Button(String(localized: "添加供应商")) { editorTarget = EditorTarget(id: "new", provider: nil) }
                        }
                    }
                    .listRowBackground(Color.clear)
                } else {
                    Section {
                        ForEach(providers) { provider in
                            row(provider)
                        }
                    } footer: {
                        Text(String(localized: "一个供应商绑定一个接口地址、一个密钥和一个模型。密钥保存在服务器，任何接口和日志都不会返回明文。"))
                    }
                }
            } else if let loadError {
                Section {
                    Text(loadError).foregroundStyle(.secondary)
                    Button(String(localized: "重试")) { Task { await load() } }
                }
            } else {
                Section { ProgressView() }
            }
        }
        .settingsPage(String(localized: "模型供应商"))
        .toolbar {
            ToolbarItem(placement: .topBarTrailing) {
                Button {
                    editorTarget = EditorTarget(id: "new", provider: nil)
                } label: {
                    Label(String(localized: "添加供应商"), appSymbol: "plus")
                }
            }
        }
        .task { await load() }
        .sheet(item: $editorTarget) { target in
            ModelProviderEditorSheet(provider: target.provider) { saved in
                editorTarget = nil
                if saved { Task { await load() } }
            }
        }
        .confirmationDialog(
            Text(String(localized: "删除供应商")),
            isPresented: Binding(
                get: { removeTarget != nil },
                set: { if !$0 { removeTarget = nil } }
            ),
            titleVisibility: .visible,
            presenting: removeTarget
        ) { provider in
            Button(String(localized: "删除"), role: .destructive) { Task { await delete(provider, force: false) } }
            Button(String(localized: "取消"), role: .cancel) { removeTarget = nil }
        } message: { _ in
            Text(String(localized: "供应商将被删除。正在使用它的会话需要重新选择。"))
        }
        .confirmationDialog(
            Text(String(localized: "供应商正在被使用")),
            isPresented: Binding(
                get: { forceDeleteTarget != nil },
                set: { if !$0 { forceDeleteTarget = nil; removeReferences = nil } }
            ),
            titleVisibility: .visible,
            presenting: forceDeleteTarget
        ) { provider in
            Button(String(localized: "仍然删除"), role: .destructive) { Task { await delete(provider, force: true) } }
            Button(String(localized: "取消"), role: .cancel) { forceDeleteTarget = nil; removeReferences = nil }
        } message: { _ in
            Text(String(localized: "有 \(removeReferences?.connectors ?? 0) 台电脑默认使用、\(removeReferences?.sessions ?? 0) 个会话单独指定了该供应商。删除后这些路由在重新选择前将没有可用的供应商。"))
        }
        .alert(alertTitle, isPresented: Binding(
            get: { checkMessage != nil }, set: { if !$0 { checkMessage = nil } }
        )) {
            Button(String(localized: "好"), role: .cancel) { checkMessage = nil }
        } message: {
            Text(checkMessage ?? "")
        }
    }

    @ViewBuilder
    private func row(_ provider: V2ModelProvider) -> some View {
        HStack(spacing: 12) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 6) {
                    Text(provider.name).font(.body.weight(.medium)).lineLimit(1)
                    Text(provider.isOpenAIProtocol ? "OpenAI" : "Anthropic")
                        .font(.caption2.weight(.medium))
                        .padding(.horizontal, 6).padding(.vertical, 2)
                        .background(.quaternary, in: Capsule())
                }
                Text(provider.modelDisplayName.isEmpty ? provider.modelId : "\(provider.modelDisplayName) · \(provider.modelId)")
                    .font(.caption).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                HStack(spacing: 8) {
                    Label(provider.credentialConfigured ? String(localized: "已配置密钥") : String(localized: "未配置密钥"),
                          appSymbol: "key")
                        .font(.caption2)
                        .foregroundStyle(provider.credentialConfigured ? Color.secondary : Color.orange)
                    if let check = provider.lastCheck {
                        Text(check.passed ? String(localized: "检查通过") : String(localized: "检查未通过"))
                            .font(.caption2)
                            .foregroundStyle(check.passed ? Color.green : Color.red)
                    }
                }
            }
            Spacer(minLength: 8)
            if busyProviderID == provider.id {
                ProgressView().controlSize(.small)
            } else {
                Toggle("", isOn: Binding(
                    get: { provider.enabled },
                    set: { enabled in Task { await setEnabled(provider, enabled) } }
                ))
                .labelsHidden()
                .disabled(busyProviderID != nil)
            }
        }
        .contentShape(Rectangle())
        .onTapGesture { editorTarget = EditorTarget(id: provider.id, provider: provider) }
        .swipeActions(edge: .trailing, allowsFullSwipe: false) {
            Button(role: .destructive) {
                removeReferences = nil
                removeTarget = provider
            } label: { Label(String(localized: "删除"), appSymbol: "trash") }
        }
        .contextMenu {
            Button {
                Task { await runCheck(provider, mode: "connect") }
            } label: { Label(String(localized: "连接测试"), appSymbol: "network") }
            Button {
                Task { await runCheck(provider, mode: "request") }
            } label: { Label(String(localized: "真实请求测试（可能产生费用）"), appSymbol: "bolt") }
        }
    }

    private func load() async {
        guard let service = appState.modelProviderService else {
            loadError = String(localized: "未登录或服务器不可用"); return
        }
        do {
            providers = try await service.providers()
            loadError = nil
        } catch {
            if providers == nil { loadError = error.localizedDescription }
        }
    }

    private func setEnabled(_ provider: V2ModelProvider, _ enabled: Bool) async {
        guard let service = appState.modelProviderService, busyProviderID == nil else { return }
        busyProviderID = provider.id
        defer { busyProviderID = nil }
        do {
            let updated = try await service.update(provider.id, .init(
                name: nil, protocolName: nil, baseUrl: nil, authType: nil, modelId: nil,
                modelDisplayName: nil, enabled: enabled, credential: nil, clearCredential: nil,
                expectedRevision: provider.revision))
            if let index = providers?.firstIndex(where: { $0.id == updated.id }) {
                providers?[index] = updated
            }
        } catch {
            alertTitle = String(localized: "操作失败")
            checkMessage = serverMessage(error)
            await load()
        }
    }

    private func runCheck(_ provider: V2ModelProvider, mode: String) async {
        guard let service = appState.modelProviderService, busyProviderID == nil else { return }
        busyProviderID = provider.id
        defer { busyProviderID = nil }
        do {
            alertTitle = String(localized: "检查结果")
            let check = try await service.check(provider.id, mode: mode)
            var lines = [check.passed ? String(localized: "检查通过") : String(localized: "检查未通过")]
            if let status = check.status { lines.append(String(localized: "状态码 \(status)")) }
            if let detail = check.detail, !detail.isEmpty { lines.append(detail) }
            checkMessage = lines.joined(separator: "\n")
            await load()
        } catch {
            alertTitle = String(localized: "操作失败")
            checkMessage = serverMessage(error)
        }
    }

    private func delete(_ provider: V2ModelProvider, force: Bool) async {
        guard let service = appState.modelProviderService else {
            alertTitle = String(localized: "操作失败")
            checkMessage = String(localized: "未登录或服务器不可用")
            return
        }
        do {
            try await service.delete(provider.id, force: force)
            removeTarget = nil
            forceDeleteTarget = nil
            removeReferences = nil
            await load()
        } catch let error as HTTPError {
            // A referenced provider answers 409 with the counts; show them and
            // require an explicit second confirmation before force-deleting.
            if error.statusCode == 409,
               let detail = error.detailValue,
               detail["code"]?.stringValue == "provider_referenced" {
                removeReferences = ReferenceCounts(
                    connectors: detail["connectorDefaults"]?.intValue ?? 0,
                    sessions: detail["explicitRoutes"]?.intValue ?? 0)
                removeTarget = nil
                // SwiftUI can drop a dialog presented in the same run loop a
                // previous dialog is dismissing; a short beat keeps the
                // force-delete confirmation reliable.
                Task {
                    try? await Task.sleep(for: .milliseconds(400))
                    forceDeleteTarget = provider
                }
                return
            }
            removeTarget = nil
            alertTitle = String(localized: "操作失败")
            checkMessage = serverMessage(error)
        } catch {
            removeTarget = nil
            alertTitle = String(localized: "操作失败")
            checkMessage = error.localizedDescription
        }
    }

    private func serverMessage(_ error: Error) -> String {
        if let http = error as? HTTPError, let detail = http.detailValue,
           let message = detail["message"]?.stringValue { return message }
        return error.localizedDescription
    }
}

// MARK: - Editor

private struct ModelProviderEditorSheet: View {
    @Environment(AppState.self) private var appState

    let provider: V2ModelProvider?
    let onFinish: (Bool) -> Void

    @State private var name: String
    @State private var protocolName: String
    @State private var baseUrl: String
    @State private var authType: String
    @State private var modelId: String
    @State private var modelDisplayName: String
    @State private var credential = ""
    @State private var clearCredential = false
    @State private var showCredential = false
    @State private var saving = false
    @State private var errorMessage: String?

    init(provider: V2ModelProvider?, onFinish: @escaping (Bool) -> Void) {
        self.provider = provider
        self.onFinish = onFinish
        _name = State(initialValue: provider?.name ?? "")
        _protocolName = State(initialValue: provider?.protocolName ?? V2ModelProvider.anthropicProtocol)
        _baseUrl = State(initialValue: provider?.baseUrl ?? "")
        _authType = State(initialValue: provider?.authType ?? "api_key")
        _modelId = State(initialValue: provider?.modelId ?? "")
        _modelDisplayName = State(initialValue: provider?.modelDisplayName ?? "")
    }

    private var isEditing: Bool { provider != nil }
    private var canSave: Bool {
        !saving && !name.trimmingCharacters(in: .whitespaces).isEmpty
            && !baseUrl.trimmingCharacters(in: .whitespaces).isEmpty
            && !modelId.trimmingCharacters(in: .whitespaces).isEmpty
    }

    var body: some View {
        NavigationStack {
            Form {
                Section {
                    TextField(String(localized: "名称"), text: $name)
                    Picker(String(localized: "协议"), selection: $protocolName) {
                        Text("Anthropic Messages").tag(V2ModelProvider.anthropicProtocol)
                        Text("OpenAI Chat Completions").tag(V2ModelProvider.openAIProtocol)
                    }
                }
                Section {
                    TextField("https://api.example.com", text: $baseUrl)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                        .keyboardType(.URL)
                    Picker(String(localized: "认证方式"), selection: $authType) {
                        Text("API Key（x-api-key）").tag("api_key")
                        Text("Bearer Token").tag("bearer")
                    }
                } header: {
                    Text(String(localized: "接口地址"))
                } footer: {
                    Text(String(localized: "会自动补上 /v1/messages（或 /v1/chat/completions）。"))
                }
                Section {
                    TextField(String(localized: "模型 ID"), text: $modelId)
                        .textInputAutocapitalization(.never).autocorrectionDisabled()
                        .font(.callout.monospaced())
                    TextField(String(localized: "模型显示名"), text: $modelDisplayName)
                }
                Section {
                    HStack {
                        Group {
                            if showCredential {
                                TextField(credentialPlaceholder, text: $credential)
                            } else {
                                SecureField(credentialPlaceholder, text: $credential)
                            }
                        }
                        .textContentType(.password).autocorrectionDisabled().textInputAutocapitalization(.never)
                        .disabled(clearCredential)
                        Button {
                            showCredential.toggle()
                        } label: {
                            AppSymbol(showCredential ? "eye.slash" : "eye").foregroundStyle(.secondary)
                        }
                        .buttonStyle(.plain)
                    }
                    if isEditing, provider?.credentialConfigured == true {
                        Toggle(String(localized: "清除已保存的密钥"), isOn: $clearCredential)
                            .tint(.red)
                    }
                } header: {
                    Text(String(localized: "密钥"))
                } footer: {
                    Text(isEditing && provider?.credentialConfigured == true
                         ? String(localized: "已配置 — 留空表示保留现有密钥。密钥保存在服务器上，不会再返回。")
                         : String(localized: "密钥保存在服务器上，任何接口和日志都不会返回明文。"))
                }
                if let errorMessage {
                    Section { Text(errorMessage).foregroundStyle(.red) }
                }
            }
            .navigationTitle(isEditing ? String(localized: "编辑供应商") : String(localized: "添加模型供应商"))
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                SheetCloseToolbar(disabled: saving) { onFinish(false) }
                SheetSaveToolbar(isWorking: saving, saveDisabled: !canSave, onSave: save)
            }
            .interactiveDismissDisabled(saving)
        }
        .appSheetPresentation(.compact)
    }

    private var credentialPlaceholder: String {
        isEditing && provider?.credentialConfigured == true
            ? String(localized: "已配置 — 留空表示保留现有密钥")
            : String(localized: "粘贴供应商密钥")
    }

    private func save() {
        guard !saving else { return }
        guard let service = appState.modelProviderService else {
            errorMessage = String(localized: "未登录或服务器不可用")
            return
        }
        saving = true
        errorMessage = nil
        let trimmedName = name.trimmingCharacters(in: .whitespaces)
        let trimmedURL = baseUrl.trimmingCharacters(in: .whitespaces)
        let trimmedModel = modelId.trimmingCharacters(in: .whitespaces)
        let trimmedDisplay = modelDisplayName.trimmingCharacters(in: .whitespaces)
        Task {
            defer { saving = false }
            do {
                if let provider {
                    _ = try await service.update(provider.id, .init(
                        name: trimmedName,
                        protocolName: protocolName,
                        baseUrl: trimmedURL,
                        authType: authType,
                        modelId: trimmedModel,
                        modelDisplayName: trimmedDisplay,
                        enabled: nil,
                        credential: credential.isEmpty || clearCredential ? nil : credential,
                        clearCredential: clearCredential ? true : nil,
                        expectedRevision: provider.revision))
                } else {
                    _ = try await service.create(.init(
                        name: trimmedName,
                        protocolName: protocolName,
                        baseUrl: trimmedURL,
                        authType: authType,
                        modelId: trimmedModel,
                        modelDisplayName: trimmedDisplay.isEmpty ? nil : trimmedDisplay,
                        credential: credential.isEmpty ? nil : credential,
                        enabled: true))
                }
                credential = ""
                onFinish(true)
            } catch let error as HTTPError where error.statusCode == 409 {
                errorMessage = String(localized: "配置已被其他设备修改，请关闭后刷新重试。")
            } catch {
                errorMessage = error.localizedDescription
            }
        }
    }
}
