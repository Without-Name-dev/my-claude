import SwiftUI
import PhotosUI
import UniformTypeIdentifiers

/// Both New Session and existing sessions use the same persistent editor and
/// picker hosts. Only the commit action and capability facts differ.
struct ChatComposerDock: View {
    @Bindable var draft: ComposerDraft
    let settings: ConversationSettings
    let maximumEditorHeight: CGFloat
    let controls: ChatControlMetrics
    var canSend = true
    var canAttach = true
    var canSelectModel = true
    var canSelectPermission = true
    var isStreaming = false
    var canStop = true
    var isBusy = false
    var sendUnavailableReason: String?
    var placeholder = String(localized: "询问 Agents")
    var isLoadingSettings = false
    var settingsError: String?
    var sessionChat: SessionChatModel?
    let onSend: (String) async -> Void
    var onStop: () async -> Void = {}
    var onLoadSettings: () async -> Void = {}
    var onApplySettings: () async -> Bool = { true }
    var applyError: () -> String? = { nil }
    var onDraftChange: () -> Void = {}

    @State private var editor = ComposerEditorController()
    @State private var showsOptions = false
    @State private var pendingPicker: AttachmentPicker?
    @State private var picker: AttachmentPicker?
    @State private var photos: [PhotosPickerItem] = []
    @State private var attachmentError: String?
    @State private var isPreparingSend = false
    @State private var importCount = 0
    @State private var showsCommandMenu = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    private enum AttachmentPicker { case photos, files }

    /// Approximate panel visibility for the container's insertion animation:
    /// the panel opens on a leading slash or the ⌘ button.
    private var commandPanelRough: Bool {
        showsCommandMenu || draft.text.hasPrefix("/")
    }

    var body: some View {
        VStack(spacing: 0) {
            if let sessionChat {
                CommandSuggestionPanel(chat: sessionChat, draft: draft, forced: $showsCommandMenu)
                    .transition(.move(edge: .bottom).combined(with: .opacity))
            }
            ChatComposer(draft: draft, editor: editor, isStreaming: isStreaming,
                canSend: canSend && importCount == 0, canStop: canStop, isBusy: isBusy || isPreparingSend,
                isImporting: importCount > 0,
                sendUnavailableReason: sendUnavailableReason,
                placeholder: placeholder,
                maximumEditorHeight: maximumEditorHeight, controls: controls,
                onSend: send, onStop: { Task { await onStop() } },
                onOptions: { showsOptions = true },
                showsCommands: sessionChat?.offersCommands == true, commandsActive: showsCommandMenu,
                onCommands: { showsCommandMenu.toggle() },
                onDraftChange: onDraftChange)
        }
        .animation(reduceMotion ? nil : .smooth(duration: 0.2), value: commandPanelRough)
        .onChange(of: sessionChat?.optionsRequest) { _, _ in showsOptions = true }
        .frame(maxWidth: ChatControlMetrics.maximumContentWidth)
        .frame(maxWidth: .infinity)
        .background {
            ZStack {
                Color.clear.photosPicker(isPresented: pickerBinding(.photos), selection: $photos,
                    maxSelectionCount: max(1, 5 - draft.attachments.count), matching: .images)
                Color.clear.fileImporter(isPresented: pickerBinding(.files), allowedContentTypes: [.item],
                    allowsMultipleSelection: true, onCompletion: importFiles)
            }
        }
        .sheet(isPresented: $showsOptions, onDismiss: presentPicker) {
            ComposerOptionsSheet(settings: settings,
                onPhotos: { queue(.photos) }, onFiles: { queue(.files) },
                canAttach: canAttach && draft.attachments.count < 5 && importCount == 0,
                attachDisabledReason: draft.attachments.count >= 5
                    ? String(localized: "每条消息最多添加 5 个附件。")
                    : (importCount > 0 ? String(localized: "附件正在导入，请稍候。") : nil),
                canSelectModel: canSelectModel, canSelectPermission: canSelectPermission,
                isLoading: isLoadingSettings, loadingError: settingsError,
                onReload: onLoadSettings, onApply: onApplySettings, applyError: applyError, sessionChat: sessionChat)
                .task(id: "\(canSelectModel):\(canSelectPermission)") { await onLoadSettings() }
        }
        .onChange(of: photos) { _, items in importPhotos(items) }
        .onDisappear {
            editor.finishEditing()
            draft.isFocused = false
        }
        .alert(String(localized: "无法添加附件"), isPresented: Binding(get: { attachmentError != nil }, set: { if !$0 { attachmentError = nil } })) {
            Button(String(localized: "好"), role: .cancel) { attachmentError = nil }
        } message: {
            Text("\(attachmentError ?? "")\n\n\(String(localized: "已添加的附件会保留。"))")
        }
    }

    private func send() {
        guard !isPreparingSend, !isBusy, importCount == 0, canSend else { return }
        isPreparingSend = true
        Task { @MainActor in
            guard let text = await editor.committedTextForSend() else {
                isPreparingSend = false
                return
            }
            isPreparingSend = false
            await onSend(text)
        }
    }

    private func queue(_ next: AttachmentPicker) { pendingPicker = next; showsOptions = false }
    private func presentPicker() {
        guard let next = pendingPicker else { return }
        pendingPicker = nil; picker = next
    }
    private func pickerBinding(_ value: AttachmentPicker) -> Binding<Bool> {
        Binding(get: { picker == value }, set: { if $0 { picker = value } else if picker == value { picker = nil } })
    }

    private func append(name: String, data: Data, mediaType: String) async -> String? {
        guard draft.isValid else { return nil }
        guard draft.attachments.count < 5 else { return String(localized: "每条消息最多添加 5 个附件。") }
        guard !data.isEmpty else { return String(localized: "文件为空。") }
        guard data.count <= 25 * 1024 * 1024 else { return String(localized: "单个附件请控制在 25 MiB 以内。") }
        let preview = mediaType.hasPrefix("image/")
            ? await Task.detached(priority: .utility) { ChatImageThumbnail.make(data: data) }.value : nil
        guard draft.isValid else { return nil }
        guard draft.attachments.count < 5 else { return String(localized: "每条消息最多添加 5 个附件。") }
        draft.attachments.append(ChatAttachment(name: name, data: data, mediaType: mediaType, previewData: preview))
        return nil
    }

    private func importPhotos(_ items: [PhotosPickerItem]) {
        guard !items.isEmpty else { return }
        importCount += 1
        Task { @MainActor in
            defer { importCount -= 1; photos = [] }
            var failures: [String] = []
            for item in items {
                do {
                    guard let data = try await item.loadTransferable(type: Data.self) else {
                        failures.append(String(localized: "有一张照片无法读取。"))
                        continue
                    }
                    let type = item.supportedContentTypes.first ?? .jpeg
                    if let failure = await append(name: "Photo-\(UUID().uuidString.prefix(8)).\(type.preferredFilenameExtension ?? "jpg")",
                           data: data, mediaType: type.preferredMIMEType ?? "image/jpeg") {
                        failures.append(failure)
                    }
                } catch { failures.append(error.localizedDescription) }
            }
            if draft.isValid, !failures.isEmpty { attachmentError = failures.joined(separator: "\n") }
        }
    }

    private func importFiles(_ result: Result<[URL], Error>) {
        do {
            let urls = try result.get()
            importCount += 1
            Task { @MainActor in
                defer { importCount -= 1 }
                var failures: [String] = []
                for (index, url) in urls.enumerated() {
                    guard draft.isValid else { return }
                    if draft.attachments.count >= 5 {
                        let skipped = urls.count - index
                        failures.append(String(localized: "已达到 5 个附件的上限，另有 \(skipped) 个文件未添加。"))
                        break
                    }
                    do {
                        let file = try await Task.detached(priority: .userInitiated) { try ImportedChatFile.read(url) }.value
                        if let failure = await append(name: file.name, data: file.data, mediaType: file.mediaType) {
                            failures.append("\(file.name)：\(failure)")
                        }
                    } catch { failures.append("\(url.lastPathComponent)：\(error.localizedDescription)") }
                }
                if draft.isValid, !failures.isEmpty { attachmentError = failures.joined(separator: "\n") }
            }
        } catch { attachmentError = error.localizedDescription }
    }
}

nonisolated private struct ImportedChatFile: Sendable {
    let name: String
    let data: Data
    let mediaType: String

    static func read(_ url: URL) throws -> Self {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        let handle = try FileHandle(forReadingFrom: url)
        defer { try? handle.close() }
        // Bound the read even when the document provider has no file-size metadata.
        let data = try handle.read(upToCount: 25 * 1024 * 1024 + 1) ?? Data()
        let type = try url.resourceValues(forKeys: [.contentTypeKey]).contentType
        return Self(name: url.lastPathComponent, data: data, mediaType: type?.preferredMIMEType ?? "application/octet-stream")
    }
}
