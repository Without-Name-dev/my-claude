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
    /// Which attachment the current download belongs to. Tapping the same
    /// attachment again is ignored instead of restarting the transfer.
    @State private var downloadingFileID: String?
    @State private var downloadID: UUID?
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    /// The in-flight preview download. A new tap replaces it instead of being
    /// dropped, so quickly opening a second attachment from the same message
    /// does not need a second tap after the first finishes.
    @State private var downloadTask: Task<Void, Never>?
    @State private var downloadTimeoutTask: Task<Void, Never>?
    @State private var toasts = ChatToastStore()
    @State private var confirmsOpenOnComputer = false
    @State private var confirmsCloseTerminal = false
    @State private var hasStartedLoading = false
    @Environment(\.colorScheme) private var colorScheme
    @ScaledMetric(relativeTo: .body) private var bodyLineHeight: CGFloat = 22

    init(session: V2SessionModel, services: V2ClientServices, deviceName: String?,
         onMenu: @escaping () -> Void) {
        _storage = StateObject(wrappedValue: StableViewModel {
            let chat = SessionChatModel(session: session, repository: services.sessionRepository, attachments: services.attachments,
                inflight: services.attachmentInflight, files: services.workspaceFiles)
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
    /// Identities of the cards the interaction dock shows. Safe-area insets can
    /// only be animated from the container that owns them.
    private var interactionNoticeIDs: [String] {
        session.notices.notices.filter { $0.blocks(session.id) }.map(\.id)
    }
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
                    if model.isOpeningReady, model.openingError == nil, session.failure == nil,
                       model.timeline.rows.isEmpty, model.timeline.pendingMessages.isEmpty {
                        VStack(spacing: 12) {
                            Text(String(localized: "在这里继续你的任务")).foregroundStyle(.secondary)
                        }.allowsHitTesting(false)
                    }
                }
                .overlay { if !model.isOpeningReady { openingMask } }
                .animation(reduceMotion ? nil : .easeOut(duration: 0.2), value: model.isOpeningReady)
                .overlay(alignment: .bottom) {
                    // A tap on an attachment used to look dead until the whole
                    // file arrived. The last-tap-wins behaviour stays, but the
                    // transfer itself is visible now.
                    if isDownloading {
                        HStack(spacing: 10) {
                            ProgressView().controlSize(.small)
                            Text(String(localized: "正在加载附件…")).font(.footnote)
                            Button {
                                if let fileID = downloadingFileID { model.cancelDownload(fileID) }
                                cancelPreviewDownload()
                            } label: {
                                AppSymbol("xmark", size: 12).frame(width: 36, height: 36).contentShape(Rectangle())
                            }
                            .buttonStyle(.plain)
                            .accessibilityLabel(String(localized: "取消加载附件"))
                        }
                        .padding(.horizontal, 16).padding(.vertical, 10)
                        .glassEffect(.regular, in: .capsule)
                        .padding(.bottom, 12)
                        .transition(.move(edge: .bottom).combined(with: .opacity))
                    }
                }
                .animation(reduceMotion ? nil : .smooth(duration: 0.2), value: isDownloading)
                .safeAreaInset(edge: .bottom, spacing: 0) {
                    VStack(spacing: 0) {
                        SessionInteractionDock(chat: model,
                            onShowAll: { expandedNoticeID = $0; sheet = .notices })
                        ChatComposerDock(draft: session.composer, settings: model.settings,
                            // A constant, not a fraction of the live geometry:
                            // the keyboard shrinks that geometry, which used to
                            // re-measure the editor cap mid-keyboard and resize
                            // the composer a second time during the animation.
                            maximumEditorHeight: 160, controls: controls,
                            canSend: session.canSend && !model.dispatchingSend, canAttach: model.canAttach,
                            canSelectModel: session.runtime.allows("catalog.model"),
                            canSelectPermission: session.runtime.allows("catalog.permission"),
                            isStreaming: model.isRunning, canStop: session.runtime.allows("session.interrupt"),
                            isBusy: model.isWorking || !model.isOpeningReady,
                            sendUnavailableReason: session.notices.notices.contains { $0.blocks(session.id) && !$0.isStale() }
                                ? String(localized: "请先处理待确认事项，草稿会保留。")
                                : String(localized: "会话暂时无法发送，草稿会保留。"),
                            placeholder: String(localized: "询问 Agents"),
                            isLoadingSettings: model.isLoadingSettings,
                            settingsError: model.settingsError, sessionChat: model,
                            onSend: model.send, onStop: model.interrupt, onLoadSettings: model.loadSettings,
                            onApplySettings: model.applySettings, applyError: { model.settingsError },
                            onDraftChange: { model.session.draftDidChange() })
                    }
                    .frame(maxWidth: ChatControlMetrics.maximumContentWidth).frame(maxWidth: .infinity)
                    // An approval dock appears without warning; animating the
                    // inset keeps the transcript from losing ~190 pt in one
                    // frame while the person is reading it.
                    .animation(reduceMotion ? nil : .smooth(duration: 0.26), value: interactionNoticeIDs)
                    // Native safe-area layout owns both the visible scroll
                    // region and the dock's space; do not add a second margin.
                }
                .overlay(alignment: .topLeading) {
                    VStack(alignment: .leading, spacing: 4) {
                        // Metadata can arrive after history. Transient controls
                        // float below the header instead of resizing its inset.
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
                    }
                    .padding(.top, 8)
                    // Banners are read while the person is acting; sliding them
                    // in and out is easier to follow than a one-frame swap.
                    .animation(reduceMotion ? nil : .smooth(duration: 0.22), value: toasts.items.map(\.id))
                    .animation(reduceMotion ? nil : .smooth(duration: 0.22), value: model.commandSuccess?.id)
                    .animation(reduceMotion ? nil : .smooth(duration: 0.22), value: model.openOnComputerConfirmation?.id)
                    .animation(reduceMotion ? nil : .smooth(duration: 0.22), value: model.closeTerminalConfirmation?.id)
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
                    Button(String(localized: "复制会话 ID"), systemImage: "number") { CopyAction.copy(session.id) }
                } label: { AppSymbol("ellipsis") }
                .accessibilityLabel(String(localized: "会话菜单"))
            }
        }
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
        .onDisappear {
            // A download that outlived the page wrote a temp directory that
            // nothing would ever remove.
            cancelPreviewDownload()
            if previewURL == nil { cleanPreview() }
        }
        .onChange(of: session.failure, initial: true) { _, failure in
            toasts.update(source: "session", failure: failure, canRetry: failure?.kind != .authentication)
        }
        // Send failures are held separately: the repository emit that follows
        // a send publishes no error, which erased the session failure before
        // the toast rendered. A failed send must not look like nothing happened.
        .onChange(of: session.actionFailure, initial: true) { _, failure in
            toasts.update(source: "send", failure: failure, canRetry: true)
        }
        .onChange(of: model.error, initial: true) { _, message in
            toasts.update(source: "operation", failure: message.map { V2ClientFailure(kind: .rejected, message: $0) })
        }
        .onChange(of: session.restoreNotice, initial: true) { _, notice in
            // restoreNotice lives outside `failure` on purpose; emit() resets
            // that field in the same turn the restore notice is written.
            toasts.update(source: "restore", failure: notice.map { V2ClientFailure(kind: .unavailable, message: $0) })
            if notice != nil { model.session.clearRestoreNotice() }
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
        .transition(.opacity)
    }

    private func openFile(_ path: String) {
        guard session.isValid, !path.isEmpty else { return }
        cancelPreviewDownload()
        sheet = .preview(SessionFileReference.parse(path))
    }

    private func openAttachment(_ file: V2AttachmentContent) {
        if file.readsFromDevice, let path = file.devicePath {
            cancelPreviewDownload()
            sheet = .preview(SessionFileReference(path: path), root: file.root)
            return
        }
        // An image can be inspected while its upload is still in flight.
        if let localID = file.fileId, localID.hasPrefix("local:") {
            cancelPreviewDownload()
            if file.isImage,
               let attachment = session.pendingMessages.lazy.flatMap(\.attachments)
                    .first(where: { "local:\($0.id)" == localID }),
               let image = UIImage(data: attachment.data) {
                previewImage = ImagePreviewItem(image: image)
                return
            }
            model.error = String(localized: "附件还没有上传完成，暂时无法预览。")
            return
        }
        guard let fileID = file.fileId else {
            cancelPreviewDownload()
            // Without an id there is nothing to fetch; say so instead of
            // swallowing the tap.
            model.error = String(localized: "这个附件暂时无法预览。")
            return
        }
        if file.isImage, let data = session.attachmentPreviews.original(for: file), let image = UIImage(data: data) {
            cancelPreviewDownload()
            previewImage = ImagePreviewItem(image: image)
            return
        }
        // Repeating the tap on the attachment that is already downloading
        // restarted the transfer; on a slow link that could keep it from ever
        // finishing. A different attachment still wins over the old one.
        if downloadingFileID == fileID { return }
        // Last tap wins: a preview that is still downloading must not swallow
        // the next tap.
        cancelPreviewDownload()
        let requestID = UUID()
        downloadID = requestID
        downloadingFileID = fileID
        isDownloading = true
        model.error = nil
        toasts.update(source: "download", failure: nil)
        let budget = 30.0 + Double(file.size ?? 0) / 200_000.0
        downloadTimeoutTask = Task { @MainActor in
            do { try await Task.sleep(for: .seconds(budget)) } catch { return }
            guard downloadID == requestID else { return }
            model.cancelDownload(fileID)
            cancelPreviewDownload()
            model.error = String(localized: "附件加载超时，请稍后重试。")
        }
        downloadTask = Task { @MainActor in
            defer {
                // A newer tap owns the state now; do not clear its indicator.
                if downloadID == requestID {
                    downloadTimeoutTask?.cancel()
                    downloadTimeoutTask = nil
                    isDownloading = false
                    downloadingFileID = nil
                    downloadID = nil
                    toasts.update(source: "download", failure: nil)
                }
            }
            do {
                // The separate timeout can end the visible wait even when a
                // shared thumbnail download is slow to respond to cancellation.
                let data = try await model.download(fileID)
                guard !Task.isCancelled, downloadID == requestID, session.isValid, sheet == nil else { return }
                if file.isImage, let image = UIImage(data: data) {
                    session.attachmentPreviews.cacheOriginal(data, for: file)
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
            } catch is CancellationError {
                // Replaced by a newer tap; its own task owns the state now.
            } catch let error as URLError where error.code == .cancelled {
                // URLSession reports task cancellation this way.
            } catch let error as URLError where error.code == .timedOut {
                guard !Task.isCancelled, downloadID == requestID else { return }
                model.error = String(localized: "附件加载超时，请稍后重试。")
            } catch {
                guard !Task.isCancelled, downloadID == requestID else { return }
                toasts.update(source: "download", failure: nil)
                model.error = error.localizedDescription
            }
        }
    }
    private func cleanPreview() {
        if let directory = previewDirectory { try? FileManager.default.removeItem(at: directory) }
        previewDirectory = nil
    }

    private func cancelPreviewDownload() {
        downloadTimeoutTask?.cancel()
        downloadTimeoutTask = nil
        downloadTask?.cancel()
        downloadTask = nil
        downloadID = nil
        downloadingFileID = nil
        isDownloading = false
    }
}

/// Command success is transient, like the Web/Desktop toast; failures use the
/// dismissible error toasts instead.
private struct CommandSuccessToast: View {
    let feedback: CommandFeedback
    let onDismiss: () -> Void
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

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
        .transition(.move(edge: .top).combined(with: .opacity))
        .task(id: feedback.id) {
            try? await Task.sleep(for: .seconds(4))
            if !Task.isCancelled {
                withAnimation(reduceMotion ? nil : .smooth(duration: 0.22)) { onDismiss() }
            }
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

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()
            ZoomableMarkdownImage(image: image)
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
