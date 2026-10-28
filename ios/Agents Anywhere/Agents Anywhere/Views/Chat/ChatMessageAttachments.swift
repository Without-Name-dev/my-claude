import SwiftUI
import UIKit

struct ChatMessageAttachments: View {
    let files: [ChatMessageAttachment]
    let onOpen: (V2AttachmentContent) -> Void
    let loadThumbnail: (V2AttachmentContent) async throws -> Data?
    var alignment: HorizontalAlignment = .trailing

    var body: some View {
        VStack(alignment: alignment, spacing: 8) {
            ForEach(files) { file in
                if file.content.isImage {
                    ChatMessageImage(file: file, onOpen: onOpen, loadThumbnail: loadThumbnail)
                } else {
                    Button { onOpen(file.content) } label: {
                        HStack(spacing: 12) {
                            AppSymbol(AppFileSymbol.name(for: file.content.name ?? ""), size: 24).frame(width: 30)
                            VStack(alignment: .leading, spacing: 3) {
                                Text(file.content.name ?? String(localized: "附件")).font(.subheadline.weight(.medium)).lineLimit(2)
                                Text(description(file.content)).font(.caption).foregroundStyle(.secondary)
                            }.frame(maxWidth: .infinity, alignment: .leading)
                            AppSymbol("arrow.up.right", size: 14).foregroundStyle(.secondary)
                        }.padding(12).frame(maxWidth: 320, alignment: .leading)
                            .background(.quaternary.opacity(0.6), in: .rect(cornerRadius: 16))
                            .contentShape(.rect(cornerRadius: 16))
                    }.buttonStyle(.plain)
                }
            }
        }
    }
    private func description(_ file: V2AttachmentContent) -> String {
        let ext = ((file.name ?? "") as NSString).pathExtension.uppercased()
        return [ext.isEmpty ? String(localized: "文件") : ext, file.size.map { ByteCountFormatter.string(fromByteCount: Int64($0), countStyle: .file) }]
            .compactMap { $0 }.joined(separator: " · ")
    }
}

private struct ChatMessageImage: View {
    let file: ChatMessageAttachment
    let onOpen: (V2AttachmentContent) -> Void
    let loadThumbnail: (V2AttachmentContent) async throws -> Data?
    @State private var image: UIImage?
    @State private var visible = false
    @State private var failed = false
    @State private var retry = 0

    /// Bounds for the rendered image; the ratio comes from the decoded image so
    /// screenshots keep their original shape instead of being cropped into a
    /// fixed 4:3 tile.
    private static let maximumWidth: CGFloat = 240
    private static let maximumHeight: CGFloat = 320

    private var displaySize: CGSize {
        guard let image, image.size.width > 0, image.size.height > 0 else {
            return CGSize(width: Self.maximumWidth, height: 180)
        }
        let ratio = image.size.width / image.size.height
        var width = Self.maximumWidth
        var height = width / ratio
        if height > Self.maximumHeight {
            height = Self.maximumHeight
            width = height * ratio
        }
        // Never upscale a small image past its native size.
        let nativeWidth = image.size.width / max(image.scale, 1)
        if width > nativeWidth {
            width = nativeWidth
            height = width / ratio
        }
        return CGSize(width: max(width, 1), height: max(height, 1))
    }

    var body: some View {
        Button {
            // A failed thumbnail must not lock the attachment: the preview
            // loads the full image and can succeed where the thumbnail did not.
            // Retrying in place was the only action, so the file could never
            // be opened from the message.
            onOpen(file.content)
        } label: {
            ZStack {
                RoundedRectangle(cornerRadius: 16).fill(.quaternary.opacity(0.6))
                if let image {
                    Image(uiImage: image).resizable().scaledToFit()
                } else if failed {
                    Label(String(localized: "轻点查看图片"), appSymbol: "photo").font(.caption).foregroundStyle(.secondary)
                } else {
                    ProgressView().progressViewStyle(.circular)
                }
            }
            .frame(width: displaySize.width, height: displaySize.height)
            .clipShape(.rect(cornerRadius: 16))
            .contentShape(.rect(cornerRadius: 16))
            // The spinner → decoded image swap used to be a hard cut.
            .animation(.easeOut(duration: 0.2), value: image != nil)
        }
        .buttonStyle(.plain).accessibilityLabel(file.content.name ?? String(localized: "图片附件"))
        .onAppear { if image == nil, let data = file.previewData { image = UIImage(data: data) } }
        .onScrollVisibilityChange(threshold: 0.01) { visible = $0 }
        .task(id: Request(visible: visible, key: file.content.cacheKey, retry: retry)) {
            guard visible, image == nil else { return }
            do {
                let data = try await loadThumbnail(file.content)
                guard !Task.isCancelled else { return }
                image = data.flatMap { UIImage(data: $0) }
                failed = image == nil
            } catch { if !Task.isCancelled { failed = true } }
        }
    }
    private struct Request: Equatable { let visible: Bool; let key: String; let retry: Int }
}

struct ChatComposerAttachment: View {
    let attachment: ChatAttachment
    let onRemove: () -> Void
    @State private var image: UIImage?
    var body: some View {
        Group {
            if attachment.isImage {
                ZStack {
                    RoundedRectangle(cornerRadius: 12).fill(.quaternary)
                    if let image { Image(uiImage: image).resizable().scaledToFill() }
                    else { AppSymbol("photo").foregroundStyle(.secondary) }
                }.frame(width: 72, height: 72).clipShape(.rect(cornerRadius: 12))
            } else {
                HStack(spacing: 8) {
                    AppSymbol(AppFileSymbol.name(for: attachment.name), size: 20)
                    VStack(alignment: .leading, spacing: 3) {
                        Text(attachment.name).font(.caption.weight(.medium)).lineLimit(2)
                        Text(ByteCountFormatter.string(fromByteCount: Int64(attachment.data.count), countStyle: .file))
                            .font(.caption2).foregroundStyle(.secondary)
                    }.frame(maxWidth: 140, alignment: .leading)
                }.padding(.horizontal, 12).padding(.trailing, 20).frame(height: 72)
                    .background(.primary.opacity(0.07), in: .rect(cornerRadius: 12))
            }
        }
        .overlay(alignment: .topTrailing) {
            // Inside the tile on purpose: the tray is a horizontal ScrollView,
            // which clips anything drawn outside the item's bounds, so an
            // offset badge never appeared.
            Button(action: onRemove) {
                AppSymbol("xmark.circle.fill").symbolRenderingMode(.palette)
                    .foregroundStyle(.white, .black.opacity(0.7)).font(.system(size: 20))
                    .frame(width: 32, height: 32).contentShape(Rectangle())
            }.buttonStyle(.plain).padding(2).accessibilityLabel(String(localized: "移除 \(attachment.name)"))
        }
        .task(id: attachment.id) {
            if let data = attachment.previewData { image = UIImage(data: data) }
        }
        .accessibilityLabel(attachment.name)
    }
}
