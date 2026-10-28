import SwiftUI
import UIKit

struct SessionTimelineRow: View, Equatable {
    let row: ChatTimelineRowModel
    let chat: SessionChatModel
    let onAttachment: (V2AttachmentContent) -> Void
    var cwd: String?
    let disclosures: TimelineDisclosureState
    let onFile: (String) -> Void
    /// Snapshot of the row's content at view construction time. The row model
    /// is a mutable reference, so comparing `row === row` alone cannot detect a
    /// content change when a parent rebuild supplies a new view value.
    private let contentRevision: Int
    @ScaledMetric(relativeTo: .body) private var lineHeight: CGFloat = 22

    init(row: ChatTimelineRowModel, chat: SessionChatModel, onAttachment: @escaping (V2AttachmentContent) -> Void,
         cwd: String? = nil, disclosures: TimelineDisclosureState, onFile: @escaping (String) -> Void) {
        self.row = row; self.chat = chat; self.onAttachment = onAttachment
        self.cwd = cwd; self.disclosures = disclosures; self.onFile = onFile
        contentRevision = row.contentRevision
    }

    /// Observation still re-evaluates this row when its own model changes; the
    /// comparison only stops sibling invalidations from reaching every row.
    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.row === rhs.row && lhs.contentRevision == rhs.contentRevision
            && lhs.chat === rhs.chat && lhs.cwd == rhs.cwd && lhs.disclosures === rhs.disclosures
    }

    var body: some View {
        Group {
            switch row.value.content {
            case let .message(message):
                let files = chat.session.attachmentPreviews.resolve(message.attachments, clientID: row.value.source["clientMessageId"]?.stringValue)
                if row.value.role == .user {
                    UserMessageBubble(text: row.text, attachments: files, onAttachment: onAttachment,
                        loadThumbnail: chat.thumbnail, dispatchStatus: row.value.status)
                } else {
                    VStack(alignment: .leading, spacing: 14) {
                        markdown
                        if !files.isEmpty {
                            ChatMessageAttachments(files: files, onOpen: onAttachment, loadThumbnail: chat.thumbnail, alignment: .leading)
                        }
                        // Waiting feedback lives in the fixed header slot. An
                        // extra placeholder row would disappear on the first token.
                        if !row.value.isStreamingText {
                            if row.value.status == .interrupted || row.value.status == .cancelled {
                                Text(String(localized: "已停止生成")).font(.caption).foregroundStyle(.secondary)
                            }
                            if row.value.status == .failed { Text(String(localized: "生成未完成")).font(.caption).foregroundStyle(.secondary) }
                        }
                    }
                }
            default:
                SessionTimelineEventView(row: row, cwd: cwd, disclosures: disclosures, onFile: onFile)

            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        // Long presses belong to the text selection interaction inside the row.
        // A row-wide context menu would intercept them and highlight the item.
    }

    private var markdown: some View {
        ChatMarkdownView(text: row.text, isStreaming: row.value.isStreamingText, resolvesFileReferences: true,
                         layoutGeneration: row.layoutGeneration)
            .frame(minHeight: row.value.isStreamingText ? lineHeight : nil, alignment: .topLeading)
    }
}

struct UserMessageBubble: View {
    let text: String
    var attachments: [ChatMessageAttachment] = []
    var onAttachment: (V2AttachmentContent) -> Void = { _ in }
    var loadThumbnail: (V2AttachmentContent) async throws -> Data? = { _ in nil }
    var isPending = false
    var dispatchStatus: V2TimelineItemStatus?
    var onDeliveryIssue: (() -> Void)?
    @Environment(\.colorScheme) private var colorScheme
    var body: some View {
        HStack(alignment: .bottom, spacing: 8) {
            Spacer(minLength: 48)
            VStack(alignment: .trailing, spacing: 8) {
                if !attachments.isEmpty {
                    ChatMessageAttachments(files: attachments, onOpen: onAttachment, loadThumbnail: loadThumbnail)
                }
                if !text.isEmpty {
                    ChatSelectableText(text: text).font(.body)
                        .padding(.horizontal, 17).padding(.vertical, 12)
                        .background(colorScheme == .dark ? Color(white: 0.13) : Color(white: 0.94), in: .rect(cornerRadius: 24))
                }
            }
            .overlay(alignment: .bottomLeading) {
                // Delivery state occupies the existing leading gutter, so the
                // echo never changes text wrapping or attachment width.
                if isPending {
                    ProgressView().progressViewStyle(.circular).controlSize(.small)
                        .frame(width: 18, height: 44).offset(x: -26).accessibilityLabel(String(localized: "正在发送消息"))
                } else if dispatchStatus == .pending || dispatchStatus == .running {
                    Text(dispatchStatus == .pending ? String(localized: "排队中") : String(localized: "处理中"))
                        .font(.caption2).foregroundStyle(.secondary).lineLimit(1).fixedSize()
                        .frame(width: 44, height: 44, alignment: .trailing).offset(x: -48)
                        .accessibilityLabel(dispatchStatus == .pending ? String(localized: "消息等待处理") : String(localized: "消息正在处理"))
                } else if dispatchStatus == .failed || dispatchStatus == .cancelled {
                    Text(dispatchStatus == .failed ? String(localized: "发送失败") : String(localized: "已取消"))
                        .font(.caption2).foregroundStyle(.red).lineLimit(1).fixedSize()
                        .frame(width: 44, height: 44, alignment: .trailing).offset(x: -48)
                } else if let onDeliveryIssue {
                    Button(action: onDeliveryIssue) {
                        AppSymbol("exclamationmark.circle").foregroundStyle(.red).frame(width: 24, height: 44)
                    }.buttonStyle(.plain).offset(x: -30).accessibilityLabel(String(localized: "查看发送问题"))
                }
            }
        }
    }
}

struct PendingMessageRow: View {
    let pending: V2PendingMessage
    let chat: SessionChatModel
    let onAttachment: (V2AttachmentContent) -> Void
    let onDismiss: () -> Void
    @State private var confirmsDismiss = false
    var body: some View {
        UserMessageBubble(text: pending.content,
            attachments: chat.session.attachmentPreviews.resolve(pending.attachments.map(\.content), clientID: pending.id),
            onAttachment: onAttachment, loadThumbnail: chat.thumbnail,
            isPending: pending.delivery == .sending,
            dispatchStatus: pending.delivery == .accepted ? .pending : nil,
            onDeliveryIssue: deliveryIssue == nil ? nil : { confirmsDismiss = true })
        .confirmationDialog(deliveryIssue ?? "", isPresented: $confirmsDismiss, titleVisibility: .visible) {
            Button(String(localized: "返回编辑")) {
                if chat.session.isLocalCreation { chat.onEditCreation?(pending) }
                // A failed send that already put its text back in the composer
                // needs no second restore; doing it again reported "the input
                // already has content" for the person's own restored message.
                else if !pending.didRestoreDraft && !chat.session.restoreDraft(from: pending) {
                    // Refused because the composer already holds a draft;
                    // saying so beats a dialog that closes and does nothing.
                    chat.error = String(localized: "输入框里已有内容。请先发送或清空，再返回这条记录。")
                }
            }
            Button(String(localized: "移除此发送记录")) {
                if chat.session.isLocalCreation { chat.onDiscardCreation?() } else { onDismiss() }
            }
            Button(String(localized: "取消"), role: .cancel) {}
        }
    }
    private var deliveryIssue: String? {
        switch pending.delivery {
        case .uncertain: String(localized: "发送结果尚未确认，草稿已保留。请先检查会话是否已收到消息；移除记录后再次发送可能产生重复消息。")
        case let .rejected(error): error.message
        default: nil
        }
    }
}

extension JSONValue {
    var readableText: String {
        if case let .string(value) = self { return value }
        let encoder = JSONEncoder(); encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
        return (try? encoder.encode(self)).flatMap { String(data: $0, encoding: .utf8) } ?? ""
    }
}
