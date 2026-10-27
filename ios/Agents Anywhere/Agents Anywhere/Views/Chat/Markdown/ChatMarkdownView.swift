import SwiftUI
import Textual

extension EnvironmentValues {
    /// One Dynamic Type step below body; every block scales from this.
    @Entry var chatMarkdownFont: Font = .callout
}

struct ChatMarkdownView: View {
    let text: String
    var isStreaming = false
    var resolvesFileReferences = false
    /// Bumped only for an authoritative replacement (not for an appended
    /// token). It resets measurement caches without changing this view's
    /// identity, so an in-progress text selection survives the update.
    var layoutGeneration = 0
    @State private var blocks: [MarkdownBlockSnapshot] = []
    /// When each block's newest glyphs finish revealing. A block's drawing
    /// clock runs only until then, so a stalled or finished block stops redrawing.
    @State private var revealDeadlines: [Int: Date] = [:]
    /// Completed blocks parsed so far; each batch re-parses only the open tail.
    @State private var parseState = MarkdownParseState()

    init(text: String, isStreaming: Bool = false, resolvesFileReferences: Bool = false, layoutGeneration: Int = 0) {
        self.text = text
        self.isStreaming = isStreaming
        self.resolvesFileReferences = resolvesFileReferences
        self.layoutGeneration = layoutGeneration
        // A settled message that was parsed before — the row left the realized
        // window and came back — is drawn at its full height on the first frame
        // instead of inflating after an asynchronous parse.
        if !isStreaming,
           let cached = ChatMarkdownCache.shared.value(for: text, resolvesFileReferences: resolvesFileReferences) {
            _blocks = State(initialValue: cached.blocks)
            _parseState = State(initialValue: cached.state)
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            ForEach(blocks) { block in
                MarkdownBlockView(block: block, isStreaming: isStreaming, isTail: block.id == blocks.last?.id,
                                  revealDeadline: revealDeadlines[block.id] ?? .distantPast,
                                  layoutGeneration: layoutGeneration)
                    .equatable()
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        // `streaming` is part of the identity on purpose. A reply that finished
        // while this row was visible parses once more when it settles, and that
        // is the parse that fills the cache; without it the row was never
        // cached, and every rebuild drew at zero height before inflating.
        .task(id: ParseRequest(text: text, resolvesFileReferences: resolvesFileReferences, streaming: isStreaming)) {
            let request = ParseRequest(text: text, resolvesFileReferences: resolvesFileReferences, streaming: isStreaming)
            if !isStreaming,
               let cached = ChatMarkdownCache.shared.value(for: text, resolvesFileReferences: resolvesFileReferences) {
                if blocks != cached.blocks { blocks = cached.blocks }
                parseState = cached.state
                return
            }
            // Parsing a long response must not block scroll and drawer gestures.
            // Cancelled requests cannot publish an older snapshot over a new reply.
            // Streaming replies reuse the blocks before the last safe boundary.
            let previous = isStreaming ? parseState : nil
            let worker = Task.detached(priority: .userInitiated) { try Self.parse(request, previous: previous) }
            let result = await withTaskCancellationHandler {
                await worker.result
            } onCancel: {
                worker.cancel()
            }
            guard !Task.isCancelled, case .success(let parsed) = result else { return }
            parseState = parsed.state
            if !isStreaming {
                ChatMarkdownCache.shared.store(
                    ChatMarkdownCache.Value(blocks: parsed.blocks, state: parsed.state),
                    for: text, resolvesFileReferences: resolvesFileReferences)
            }
            let next = parsed.blocks
            guard blocks != next else { return }
            if isStreaming {
                // Set in the same update as the text, so the first frame of new
                // glyphs already has a running clock.
                let deadline = Date.now.addingTimeInterval(ReplyPresentation.revealSeconds + ReplyPresentation.drawSlack)
                let previous = Dictionary(blocks.map { ($0.id, $0.digest) }, uniquingKeysWith: { $1 })
                var deadlines = revealDeadlines.filter { id, _ in next.contains { $0.id == id } }
                for block in next where previous[block.id] != block.digest { deadlines[block.id] = deadline }
                revealDeadlines = deadlines
            }
            blocks = next
        }
    }

    nonisolated private struct ParseRequest: Equatable, Sendable {
        let text: String
        let resolvesFileReferences: Bool
        /// Whether the reply is still growing. The final parse is the one whose
        /// result is cached, so settling must restart the task even when the
        /// text stopped changing.
        let streaming: Bool

        init(text: String, resolvesFileReferences: Bool, streaming: Bool = false) {
            self.text = text
            self.resolvesFileReferences = resolvesFileReferences
            self.streaming = streaming
        }
    }

    /// Completed text is parsed once per safe boundary; the open tail is parsed
    /// per batch. A reply whose text no longer extends the cached prefix (a
    /// correction or recovery) starts over from an empty state.
    nonisolated private static func parse(_ request: ParseRequest, previous: MarkdownParseState?)
        throws -> (blocks: [MarkdownBlockSnapshot], state: MarkdownParseState) {
        let text = request.text
        var state = previous.flatMap { $0.covers(text) ? $0 : nil } ?? MarkdownParseState()
        let consumed = state.prefixEnd(text)
        let split = MarkdownStreamSplit.safeSplitIndex(in: text).flatMap { $0 >= consumed ? $0 : nil }
        if let split, split > consumed {
            let completed = try parse(String(text[consumed..<split]), request: request, startingAt: state.nextID)
            state.blocks.append(contentsOf: completed)
            state.nextID += completed.count
            state.prefixText = String(text[..<split])
        }
        let tail = String(text[(split ?? consumed)...])
        let tailBlocks = tail.isEmpty ? [] : try parse(tail, request: request, startingAt: state.nextID)
        return (state.blocks + tailBlocks, state)
    }

    nonisolated private static func parse(_ text: String, request: ParseRequest, startingAt startID: Int) throws -> [MarkdownBlockSnapshot] {
        try Task.checkCancellation()
        var document = try AttributedStringMarkdownParser.parse(text, syntaxExtensions: [.math])
        try Task.checkCancellation()
        if request.resolvesFileReferences {
            for run in document.runs {
                if run.link == nil, run.inlinePresentationIntent?.contains(.code) == true,
                   let reference = SessionFileReference.inlineReference(String(document[run.range].characters)) {
                    document[run.range].link = reference.link
                }
            }
        }
        document = GitDirectiveParser.enrich(document) { directives, attributes in
            var badge = AttributedString(directives.map(\.label).joined(separator: " · "), attributes: attributes)
            badge.textual.attachment = AnyAttachment(ChatGitBadgeAttachment(directives: directives))
            return badge
        }
        try Task.checkCancellation()
        return MarkdownBlockSnapshot.split(document, startingAt: startID)
    }
}

extension ChatMarkdownView {
    /// Parses messages the reader is about to scroll into view.
    ///
    /// A lazily realized row whose markdown is not cached yet is laid out at
    /// zero height and inflates when its parse lands. When that row appears
    /// above the viewport, everything the person is looking at is pushed down
    /// — the jump seen when swiping up through old messages. Parsing a window
    /// around the reading position in the background means a row arrives at
    /// its final height on the first frame instead.
    nonisolated static func prewarm(_ texts: [String]) {
        guard !texts.isEmpty else { return }
        Task.detached(priority: .utility) {
            for text in texts where !text.isEmpty {
                if ChatMarkdownCache.shared.value(for: text, resolvesFileReferences: true) != nil { continue }
                let request = ParseRequest(text: text, resolvesFileReferences: true, streaming: false)
                guard let parsed = try? parse(request, previous: nil) else { continue }
                ChatMarkdownCache.shared.store(
                    ChatMarkdownCache.Value(blocks: parsed.blocks, state: parsed.state),
                    for: text, resolvesFileReferences: true)
            }
        }
    }
}

private struct MarkdownBlockView: View, Equatable {
    let block: MarkdownBlockSnapshot
    let isStreaming: Bool
    let isTail: Bool
    let revealDeadline: Date
    let layoutGeneration: Int
    @State private var hasSettled = false
    @State private var headingLedger = GlyphRevealLedger()
    @Environment(\.dynamicTypeSize) private var dynamicType
    @Environment(\.displayScale) private var displayScale
    @Environment(\.layoutDirection) private var direction
    @Environment(\.chatMarkdownFont) private var font

    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.block == rhs.block && lhs.isStreaming == rhs.isStreaming && lhs.isTail == rhs.isTail
            && lhs.revealDeadline == rhs.revealDeadline && lhs.layoutGeneration == rhs.layoutGeneration
    }

    var body: some View {
        // Only changed blocks reach Textual's parser. The complete document was
        // parsed above, so cross-block references and nested structures survive.
        MarkdownBlockLayout(dynamicType: dynamicType, displayScale: displayScale, direction: direction,
                            generation: layoutGeneration) {
            StructuredText(String(block.digest), parser: ParsedMarkdownText(content: block.content))
                .textual.structuredTextStyle(ChatMarkdownStyle(headingLedger: headingLedger))
                .textual.imageAttachmentLoader(ChatImageLoader())
                // Selection spans the whole message body. Interactive
                // attachments publish exclusion rectangles, and nested
                // code/table scroll views keep their own scoped overlays.
                .textual.textSelection(.enabled)
                .environment(\.streamingGlyphAnimation, isStreaming && !hasSettled)
                .environment(\.streamingRevealDeadline, revealDeadline)
                .font(font)
                .foregroundStyle(.primary)
                .fixedSize(horizontal: false, vertical: true)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
        .task(id: RevealPhase(isStreaming: isStreaming, isTail: isTail)) {
            // Opening static history must not schedule delayed state changes
            // for every paragraph. A live append still settles completed blocks.
            guard isStreaming else { return }
            hasSettled = false
            guard !isTail else { return }
            do { try await Task.sleep(for: ReplyPresentation.settleDelay) } catch { return }
            // Finish any new glyphs, then stop the drawing clock for this
            // completed block even while the rest of the response streams.
            hasSettled = true
        }
    }

    private struct RevealPhase: Equatable {
        let isStreaming: Bool
        let isTail: Bool
    }
}

/// Apply one complete style. A default bundle closer to StructuredText would
/// override individual styles applied outside it, silently bypassing our renderer.
private struct ChatMarkdownStyle: StructuredText.Style {
    private let defaults = StructuredText.DefaultStyle()
    let headingStyle: ChatHeadingStyle
    let paragraphStyle = ChatParagraphStyle()
    let codeBlockStyle = ChatCodeBlockStyle()

    init(headingLedger: GlyphRevealLedger) {
        headingStyle = ChatHeadingStyle(ledger: headingLedger)
    }

    var inlineStyle: InlineStyle { defaults.inlineStyle }
    var blockQuoteStyle: ChatBlockQuoteStyle { ChatBlockQuoteStyle() }
    // Textual's defaults indent each level by 2em; a phone column needs ~1.4em.
    var listItemStyle: StructuredText.DefaultListItemStyle { .default(markerSpacing: .fontScaled(0.4)) }
    var unorderedListMarker: StructuredText.SymbolListMarker {
        .init(symbolName: "circle.fill", scale: 0.33, minWidth: .fontScaled(1))
    }
    var orderedListMarker: StructuredText.DecimalListMarker { .init(minWidth: .fontScaled(1)) }
    var tableStyle: ChatTableStyle { ChatTableStyle() }
    var tableCellStyle: StructuredText.DefaultTableCellStyle { defaults.tableCellStyle }
    var thematicBreakStyle: StructuredText.DividerThematicBreakStyle { defaults.thematicBreakStyle }
}

private struct ChatParagraphStyle: StructuredText.ParagraphStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .textual.lineSpacing(.fontScaled(0.23))
            .textual.blockSpacing(.fontScaled(top: 0.8))
            .modifier(StreamingGlyphReveal())
    }
}

/// A thin leading rule, like Web, instead of Textual's padded aside box.
private struct ChatBlockQuoteStyle: StructuredText.BlockQuoteStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)
            .textual.padding(.leading, .fontScaled(0.75))
            .overlay(alignment: .leading) {
                Capsule().fill(.primary.opacity(0.18)).frame(width: 3).allowsHitTesting(false)
            }
    }
}

private struct ChatHeadingStyle: StructuredText.HeadingStyle {
    let ledger: GlyphRevealLedger

    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .textual.fontScale(configuration.headingLevel == 1 ? 1.5 : configuration.headingLevel == 2 ? 1.25 : 1.08)
            .fontWeight(.semibold)
            .textual.blockSpacing(.fontScaled(top: 1.25, bottom: 0.55))
            // Textual identifies headings by their changing slug. Keep births
            // in the stable outer block so an append doesn't replay the heading.
            .modifier(StreamingGlyphReveal(ledger: ledger))
    }
}

private struct ChatCodeBlockStyle: StructuredText.CodeBlockStyle {
    func makeBody(configuration: Configuration) -> some View {
        ChatMarkdownCodeBlock(code: configuration.codeBlock.text, language: configuration.languageHint) {
            configuration.label
                .monospaced()
                .textual.fontScale(0.86)
                .textual.lineSpacing(.fontScaled(0.35))
                .modifier(StreamingGlyphReveal())
        }
        .textual.blockSpacing(.fontScaled(top: 0.9, bottom: 0.5))
    }
}

private struct ChatTableStyle: StructuredText.TableStyle {
    func makeBody(configuration: Configuration) -> some View {
        ChatMarkdownTable(rows: configuration.rows, columns: configuration.columns)
            .textual.blockSpacing(.fontScaled(top: 0.9, bottom: 0.5))
    }
}
