import SwiftUI
import QuickLook

struct SessionChatView: View, Equatable {
    @StateObject private var storage: StableViewModel<SessionChatModel>
    private var model: SessionChatModel { storage.value }
    private let sessionIdentity: V2SessionModel
    let deviceName: String?
    let onMenu: () -> Void
    @State private var sheet: SessionSheet?
    @State private var expandedNoticeID: String?
    private let fileService: V2WorkspaceFilesService
    private let detailService: V2SessionDetailService
    private enum SessionSheet: Identifiable {
        case notices, details, files, preview(SessionFileReference, root: String? = nil)
        var id: String { switch self { case .notices: "notices"; case .details: "details"; case .files: "files"; case .preview(let reference, let root): "file:\(root ?? ""):\(reference.id)" } }
    }
    @State private var previewURL: URL?
    @State private var previewDirectory: URL?
    /// Images open in their own viewer: QuickLook needs a file name it can type
    /// from, so an attachment without an extension opened to a blank page.
    @State private var previewImage: ImagePreviewItem?
    @State private var isDownloading = false
    @State private var toasts = ChatToastStore()
    @State private var pendingTakeover: Bool?
    @State private var confirmsOpenOnComputer = false
    @State private var confirmsCloseTerminal = false
    @State private var hasStartedLoading = false
    @Environment(\.colorScheme) private var colorScheme
    @ScaledMetric(relativeTo: .body) private var bodyLineHeight: CGFloat = 22
    @ScaledMetric(relativeTo: .footnote) private var takeoverPillHeight: CGFloat = 32

    init(session: V2SessionModel, services: V2ClientServices, deviceName: String?,
         onMenu: @escaping () -> Void) {
        _storage = StateObject(wrappedValue: StableViewModel {
            let chat = SessionChatModel(session: session, repository: services.sessionRepository, attachments: services.attachments,
                files: services.workspaceFiles)
            chat.onEditCreation = { [weak services, weak session] pending in
                if let session { services?.editCreation(session, pending: pending) }
            }
            chat.onDiscardCreation = { [weak services] in services?.discardCreation(session.id) }
            return chat
        })
        sessionIdentity = session
        self.deviceName = deviceName
        fileService = services.workspaceFiles; detailService = services.sessionDetail
        self.onMenu = onMenu
    }
    private var controls: ChatControlMetrics { .init(bodyLineHeight: bodyLineHeight) }
    private var session: V2SessionModel { model.session }
    private var requiresTakeover: Bool { session.metadata?.takeover == false }
    // Sidebar motion changes the containing card, not the session. Observable
    // model changes and real size/environment changes still update this subtree.
    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.sessionIdentity === rhs.sessionIdentity && lhs.deviceName == rhs.deviceName
    }
    var body: some View {
        GeometryReader { geometry in
            Group {
                if hasStartedLoading {
                    ChatTimelineView(model: model,
                        onAttachment: openAttachment, onFile: openFile)
                } else {
                    Color.clear.frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
                .overlay {
                    if model.isOpeningReady && model.timeline.rows.isEmpty && model.timeline.pendingMessages.isEmpty {
                        VStack(spacing: 12) {
                            Text(String(localized: "在这里继续你的任务")).foregroundStyle(.secondary)
                        }.allowsHitTesting(false)
                    }
                }
                .overlay { if !model.isOpeningReady { openingMask } }
                .safeAreaInset(edge: .bottom, spacing: 0) {
                    VStack(spacing: 0) {
                        SessionInteractionDock(chat: model,
                            onShowAll: { expandedNoticeID = $0; sheet = .notices })
                        ChatComposerDock(draft: session.composer, settings: model.settings,
                            maximumEditorHeight: min(160, max(72, geometry.size.height * 0.30)), controls: controls,
                            canSend: session.canSend, canAttach: model.canAttach,
                            canSelectModel: session.runtime.allows("catalog.model"),
                            canSelectPermission: session.runtime.allows("catalog.permission"),
                            isStreaming: model.isRunning, canStop: session.runtime.allows("session.interrupt"),
                            isBusy: model.isWorking || !model.isOpeningReady, placeholder: requiresTakeover ? String(localized: "请先接管") : String(localized: "询问 Agents"),
                            isLoadingSettings: model.isLoadingSettings,
                            settingsError: model.settingsError, sessionChat: model,
                            onSend: model.send, onStop: model.interrupt, onLoadSettings: model.loadSettings,
                            onApplySettings: model.applySettings, applyError: { model.settingsError },
                            onDraftChange: { model.session.draftDidChange() })
                    }
                    .frame(maxWidth: ChatControlMetrics.maximumContentWidth).frame(maxWidth: .infinity)
                    // Native safe-area layout owns both the visible scroll
                    // region and the dock's space; do not add a second margin.
                }
                .overlay(alignment: .topLeading) {
                    VStack(alignment: .leading, spacing: 4) {
                        // Metadata can arrive after history. Transient controls
                        // float below the header instead of resizing its inset.
                        if requiresTakeover {
                            takeoverPill.frame(maxWidth: .infinity, alignment: .center)
                        }
                        ChatErrorToasts(store: toasts, isRetrying: session.isLoading,
                            onRetry: { _ in await session.refresh() },
                            onOpenOnComputer: { _ in _ = await model.openOnComputer() },
                            isOpeningOnComputer: model.isOpeningOnComputer)
                        if let success = model.commandSuccess {
                            CommandSuccessToast(feedback: success) { model.commandSuccess = nil }
                                .frame(maxWidth: .infinity, alignment: .center)
                        }
                        if let confirmation = model.openOnComputerConfirmation {
                            CommandSuccessToast(feedback: confirmation) { model.openOnComputerConfirmation = nil }
                                .frame(maxWidth: .infinity, alignment: .center)
                        }
                        if let confirmation = model.closeTerminalConfirmation {
                            CommandSuccessToast(feedback: confirmation) { model.closeTerminalConfirmation = nil }
                                .frame(maxWidth: .infinity, alignment: .center)
                        }
                    }.padding(.top, 8)
                }
        }
        .modifier(ChatPageToolbar(title: session.metadata?.title ?? String(localized: "会话"),
            subtitle: [session.metadata?.runtimeName ?? session.metadata?.runtime ?? String(localized: "Agent"),
                deviceName ?? session.metadata?.connectorId].compactMap { $0 }.joined(separator: " · "),
            status: model.headerStatus, alignsTitleLeading: true, onMenu: onMenu,
            live: session.metadata?.isLive ?? false))
        .toolbar {
            ToolbarItemGroup(placement: .topBarTrailing) {
                Button { sheet = .files } label: { AppSymbol("folder") }
                    .accessibilityLabel(String(localized: "文件管理"))
                    .disabled(session.metadata?.cwd?.isEmpty != false)
                Menu {
                    Button(String(localized: "在电脑上打开"), systemImage: "display") { confirmsOpenOnComputer = true }
                        .disabled(!model.canOpenOnComputer)
                    Button(String(localized: "关闭终端"), systemImage: "xmark.rectangle") { confirmsCloseTerminal = true }
                        .disabled(!model.canCloseTerminal)
                    Button(String(localized: "会话详情与导出"), systemImage: "info.circle") { sheet = .details }
                    Button(String(localized: "复制会话 ID"), systemImage: "number") { UIPasteboard.general.string = session.id }
                } label: { AppSymbol("ellipsis") }
                .accessibilityLabel(String(localized: "会话菜单"))
            }
        }
        .modifier(SessionTakeoverConfirmation(pending: $pendingTakeover, isDsh: model.isDsh) { enabled in
            model.error = nil
            if !(await model.setTakeover(enabled)), let error = model.takeoverError { model.error = error }
        })
        .confirmationDialog(String(localized: "关闭这个会话的终端？"),
            isPresented: $confirmsCloseTerminal, titleVisibility: .visible) {
            Button(String(localized: "关闭终端"), role: .destructive) {
                Task { await model.closeTerminal() }
            }
            Button(String(localized: "取消"), role: .cancel) {}
        } message: {
            Text(String(localized: "会结束电脑上这个会话的进程，对话记录保留；下次发送消息会自动重新打开。"))
        }
        .confirmationDialog(String(localized: "在电脑上打开此会话？"),
            isPresented: $confirmsOpenOnComputer, titleVisibility: .visible) {
            Button(String(localized: "在电脑上打开")) {
                Task { await model.openOnComputer() }
            }
            Button(String(localized: "取消"), role: .cancel) {}
        } message: {
            Text(String(localized: "将在这台电脑上启动一个终端并打开此会话。"))
        }
        .completionFeedback(trigger: model.isRunning) { wasRunning, isRunning in
            wasRunning && !isRunning && model.isOpeningReady && session.runtime.isFresh
                && session.runtime.state?.status == .idle
        }
        .sidebarDrawerSettledTask(id: hasStartedLoading) { settled in
            guard !hasStartedLoading, settled else { return }
            // Show feedback immediately, but let the drawer's completed
            // animation and the selection's final layout leave the main thread.
            do { try await Task.sleep(for: .milliseconds(120)) } catch { return }
            guard !Task.isCancelled else { return }
            var transaction = Transaction(animation: nil)
            transaction.disablesAnimations = true
            withTransaction(transaction) { hasStartedLoading = true }
        }
        .task(id: hasStartedLoading) {
            guard hasStartedLoading else { return }
            // Reattaching a loaded detail only resumes observation.
            await model.prepareOpening()
            guard !Task.isCancelled else { return }
            await model.timeline.run(sessionID: session.id, repository: model.repository)
        }
        .sheet(item: $sheet) { destination in
            switch destination {
            case .notices: SessionNoticesSheet(model: model, initialNoticeID: expandedNoticeID)
            case .details: SessionDetailsSheet(chat: model, service: detailService)
            case .files:
                if let meta = session.metadata, let cwd = meta.cwd {
                    WorkspaceFilesSheet(connectorId: meta.connectorId,
                        deviceName: deviceName ?? meta.connectorId,
                        workspace: V2DeviceWorkspace(path: cwd, name: String(localized: "会话文件"), sessionCount: 1, lastActiveAt: nil),
                        service: fileService, session: session)
                }
            case .preview(let reference, let root):
                if let meta = session.metadata {
                    WorkspaceFilePreviewSheet(connectorId: meta.connectorId, root: root ?? meta.cwd ?? ".", path: reference.path,
                        service: fileService, session: session, location: reference)
                }
            }
        }
        .environment(\.openURL, OpenURLAction { url in
            if let reference = SessionFileReference.reference(from: url) {
                if session.isValid { sheet = .preview(reference) }
                return .handled
            }
            return ["https", "http", "mailto"].contains(url.scheme?.lowercased() ?? "") ? .systemAction : .discarded
        })
        .quickLookPreview($previewURL)
        .sheet(item: $previewImage) { item in
            ChatImagePreview(image: item.image)
        }
        .onChange(of: previewURL) { _, url in if url == nil { cleanPreview() } }
        .onDisappear { if previewURL == nil { cleanPreview() } }
        .onChange(of: session.failure, initial: true) { _, failure in
            toasts.update(source: "session", failure: failure, canRetry: failure?.kind != .authentication)
        }
        .onChange(of: model.error, initial: true) { _, message in
            toasts.update(source: "operation", failure: message.map { V2ClientFailure(kind: .rejected, message: $0) })
        }
        .onChange(of: model.commandFailure) { _, feedback in
            // Each command failure is new, even when its text repeats a dismissed one.
            toasts.update(source: "command", failure: nil)
            toasts.update(source: "command", failure: feedback.map { V2ClientFailure(kind: .rejected, message: $0.message ?? "") }, title: feedback?.title)
        }
        .onChange(of: model.openingError, initial: true) { _, message in
            toasts.update(source: "opening", failure: message.map { V2ClientFailure(kind: .unavailable, message: $0) })
        }
        // A scroll trace goes to the server every 20 seconds, so a reported
        // jump can be read back without asking the person to copy anything.
        // Only new events are sent; failures are ignored.
        .task(id: session.id) {
            var cursor = 0
            while !Task.isCancelled {
                do { try await Task.sleep(for: .seconds(20)) } catch { return }
                let report = TimelineTrace.shared.text(after: cursor)
                guard !report.text.isEmpty else { continue }
                cursor = report.last
                await model.uploadDiagnostics(report.text)
            }
        }
    }

    private var openingMask: some View {
        ZStack {
            Color(uiColor: .systemBackground)
            VStack(spacing: 12) {
                if let error = model.openingError, !model.timeline.hasPresentedSnapshot {
                    Text(error).font(.subheadline).foregroundStyle(.secondary).multilineTextAlignment(.center)
                    Button(String(localized: "重试")) { Task { await model.prepareOpening() } }
                } else {
                    ProgressView().progressViewStyle(.circular).accessibilityLabel(String(localized: "正在加载会话"))
                }
            }
            .padding(24).frame(maxWidth: 320)
        }
        .transition(.identity)
    }

    private var takeoverPill: some View {
        Button { pendingTakeover = true } label: {
            HStack(spacing: 8) {
                if model.isWorking {
                    ProgressView()
                        .progressViewStyle(.circular)
                        .scaleEffect(0.8)
                        .tint(AppTheme.primaryText(colorScheme))
                }
                Label(String(localized: "接管会话以继续交互"), appSymbol: "hand.raised")
                    .font(.footnote.weight(.medium))
                    .foregroundStyle(AppTheme.primaryText(colorScheme))
            }
            .padding(.horizontal, 14).frame(minHeight: takeoverPillHeight)
            .glassEffect(.regular.interactive(), in: .capsule)
            .frame(minHeight: 44).contentShape(Rectangle())
        }
        .buttonStyle(.plain).disabled(!model.canChangeTakeover || model.isWorking)
        .accessibilityIdentifier("chat.session.takeover")
        .padding(.horizontal, 20).padding(.bottom, 4)
    }

    private func openFile(_ path: String) {
        guard session.isValid, !path.isEmpty else { return }
        sheet = .preview(SessionFileReference.parse(path))
    }

    private func openAttachment(_ file: V2AttachmentContent) {
        if file.readsFromDevice, let path = file.devicePath { sheet = .preview(SessionFileReference(path: path), root: file.root); return }
        // An optimistic attachment that has not finished uploading has only a
        // local id; asking the server for it can only 404.
        if file.fileId?.hasPrefix("local:") == true {
            model.error = String(localized: "附件还没有上传完成，暂时无法预览。")
            return
        }
        guard !isDownloading else { return }
        guard let fileID = file.fileId else {
            // Without an id there is nothing to fetch; say so instead of
            // swallowing the tap.
            model.error = String(localized: "这个附件暂时无法预览。")
            return
        }
        isDownloading = true
        model.error = nil
        // Show downloading indicator
        toasts.update(source: "download", failure: nil)
        Task { @MainActor in
            defer {
                isDownloading = false
                toasts.update(source: "download", failure: nil)
            }
            do {
                let data = try await model.download(fileID)
                guard session.isValid, sheet == nil else { return }
                if file.isImage, let image = UIImage(data: data) {
                    previewImage = ImagePreviewItem(image: image)
                    return
                }
                cleanPreview()
                let directory = FileManager.default.temporaryDirectory.appendingPathComponent("aa-preview-\(UUID().uuidString)", isDirectory: true)
                let name = (file.name ?? String(localized: "Attachment")) as NSString
                let safeName = name.lastPathComponent.isEmpty ? String(localized: "Attachment") : name.lastPathComponent
                let url = directory.appendingPathComponent(safeName)
                try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
                try data.write(to: url, options: [.atomic, .completeFileProtection])
                previewDirectory = directory; previewURL = url
            } catch {
                toasts.update(source: "download", failure: nil)
                model.error = error.localizedDescription
            }
        }
    }
    private func cleanPreview() {
        if let directory = previewDirectory { try? FileManager.default.removeItem(at: directory) }
        previewDirectory = nil
    }
}

/// Command success is transient, like the Web/Desktop toast; failures use the
/// dismissible error toasts instead.
private struct CommandSuccessToast: View {
    let feedback: CommandFeedback
    let onDismiss: () -> Void

    var body: some View {
        Button(action: onDismiss) {
            HStack(alignment: .firstTextBaseline, spacing: 8) {
                AppSymbol("checkmark.circle", size: 14).foregroundStyle(.secondary)
                VStack(alignment: .leading, spacing: 2) {
                    Text(feedback.title).font(.subheadline.weight(.medium))
                    if let message = feedback.message, !message.isEmpty {
                        Text(message).font(.footnote).foregroundStyle(.secondary).lineLimit(3)
                    }
                }
            }
            .padding(.horizontal, 16).padding(.vertical, 10)
            .glassEffect(.regular, in: .rect(cornerRadius: 20))
        }
        .buttonStyle(.plain)
        .padding(.horizontal, ChatControlMetrics.collapsedHorizontalInset)
        .accessibilityHint(String(localized: "轻点关闭"))
        .task(id: feedback.id) {
            try? await Task.sleep(for: .seconds(4))
            if !Task.isCancelled { onDismiss() }
        }
    }
}

/// One downloaded image, kept for the viewer only.
private struct ImagePreviewItem: Identifiable {
    let id = UUID()
    let image: UIImage
}

/// Full-screen image viewer for attachments and pictures in messages.
///
/// QuickLook was the previous destination, but it needs a file name it can type
/// from: an attachment stored without an extension (or with a name the system
/// does not recognise) opened to a blank page, which is why tapping a sent photo
/// looked like nothing happened.
private struct ChatImagePreview: View {
    let image: UIImage
    @Environment(\.dismiss) private var dismiss
    @State private var scale: CGFloat = 1
    @State private var settledScale: CGFloat = 1

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            Image(uiImage: image)
                .resizable()
                .scaledToFit()
                .scaleEffect(scale)
                .gesture(
                    MagnifyGesture()
                        .onChanged { value in
                            scale = min(max(settledScale * value.magnification, 1), 6)
                        }
                        .onEnded { _ in settledScale = scale }
                )
                .onTapGesture(count: 2) {
                    withAnimation(.snappy) {
                        scale = scale > 1 ? 1 : 2.5
                        settledScale = scale
                    }
                }
            VStack {
                HStack {
                    Spacer()
                    Button { dismiss() } label: {
                        AppSymbol("xmark", size: 16)
                            .foregroundStyle(.white)
                            .frame(width: 40, height: 40)
                            .background(.black.opacity(0.45), in: .circle)
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(String(localized: "关闭"))
                }
                Spacer()
            }
            .padding()
        }
        .statusBarHidden()
    }
}
