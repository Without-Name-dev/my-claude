import SwiftUI
import UIKit

struct ChatTimelineView: View {
    let model: SessionChatModel
    let onAttachment: (V2AttachmentContent) -> Void
    let onFile: (String) -> Void
    @State private var historyLayout = ChatHistoryLayoutSample()
    @State private var visibleHistoryRows = ChatVisibleHistoryRows()
    @State private var historyPosition: TimelineHistoryPosition?
    @State private var hasRequestedOlder = false
    /// Reading position the markdown prewarm last covered. Not published: it
    /// moves while the person scrolls and must not invalidate the view.
    @State private var prewarmCursor = ChatPrewarmCursor()
    @State private var position = ScrollPosition()
    @State private var scrolling = TimelineScrollState()
    @State private var viewportSample = ChatViewportSample()
    @State private var viewportUpdates = ChatLayoutUpdate<TimelineViewport>()
    @State private var historyUpdates = ChatLayoutUpdate<TimelineHistoryLayout>()
    @State private var latestPull = TimelineHistoryPull()
    @State private var olderPull = TimelineHistoryPull(edge: .older)
    @State private var olderPromptVisible = false
    @State private var olderLoadRequest: Int?
    @State private var latestPromptVisible = false
    @State private var latestLoadRequest: Int?
    /// A pull-release requested an adjacent page. It is issued after the
    /// scroll settles: inserting a page while the release bounce is still
    /// moving the offset shifts the content under the reader (a jump), and the
    /// correction can only land once the native scroll is idle.
    @State private var pendingOlderLoad = false
    @State private var pendingLatestLoad = false
    @State private var nativePhase = TimelineScrollState.Phase.idle
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.sidebarDrawerIsTransitioning) private var sidebarIsTransitioning
    @Environment(\.sidebarDrawerObscuresDetail) private var sidebarObscuresDetail
    @ScaledMetric(relativeTo: .caption) private var returnPillHeight: CGFloat = 32

    /// The same predicate the interaction dock renders with. A notice that is
    /// visible but does not block this session draws no card, so it must not
    /// suspend following either — that left a streaming reply parked with no
    /// explanation on screen.
    private var hasInteractions: Bool {
        model.session.notices.notices.contains { $0.isVisible && $0.blocks(model.session.id) }
    }
    private var viewport: TimelineViewport { viewportSample.value ?? scrolling.viewport }
    private var navigationIsSuspended: Bool { sidebarIsTransitioning || sidebarObscuresDetail }
    var body: some View {
        ScrollViewReader { scrollProxy in
            timelineBody(using: scrollProxy)
        }
    }

    private func timelineBody(using scrollProxy: ScrollViewProxy) -> some View {
        // A sibling overlay receives taps independently of the scroll view's
        // deceleration recognizer. The explicit return intent survives its callbacks.
        ZStack(alignment: .bottom) {
            ScrollView {
                ChatTimelineContent(model: model, onAttachment: onAttachment, onFile: onFile,
                    latestPullReady: latestPull.isReady, isLoadingLatest: latestLoadRequest != nil,
                    olderPullReady: olderPull.isReady, isLoadingOlder: olderLoadRequest != nil,
                    keepsOlderPrompt: hasRequestedOlder, historyAnchor: historyPosition?.origin,
                    timelineRevision: model.timeline.presentationRevision,
                    notices: model.session.notices.notices,
                    // Value snapshot of the notice state. The models are
                    // mutated in place, so comparing the objects by identity
                    // never noticed a resolution made on another device and
                    // the inline card stayed in the transcript.
                    noticeSignature: model.session.notices.notices.map {
                        "\($0.id):\($0.notice.revision):\($0.isVisible)"
                    },
                    isRunning: model.isRunning,
                    hasOlderItems: model.session.hasOlderItems,
                    hasNewerItems: model.session.hasNewerItems,
                    hasPendingUserMessage: !model.timeline.pendingMessages.isEmpty || !model.session.pendingMessages.isEmpty,
                    subagentActivity: model.timeline.subagentActivity,
                    onLoadOlder: loadOlder, onLoadLatest: loadLatest,
                    onHistoryRowLayout: { rowID, layout, frame in
                        visibleHistoryRows.update(rowID: rowID, layout: layout, frame: frame)
                        if historyPosition?.origin?.anchorRowID == layout.anchorRowID {
                            historyDidLayOut(layout)
                        }
                    },
                    onHistoryRowVisibility: { rowID, visible in
                        visibleHistoryRows.setVisible(visible, rowID: rowID)
                        prewarmAroundReader()
                    },
                    onPromptVisibility: { latestPromptVisible = $0 },
                    onOlderPromptVisibility: { olderPromptVisible = $0 },
                    onTailVisibility: { region, visible in scrolling.tailVisibilityChanged(region, visible: visible) })
                    .equatable()
                    .background {
                        ChatPageScrollEdge()
                        // Inside the content, so the recogniser it installs
                        // lands on this scroll view.
                        ChatTranscriptTapDismiss(onTap: dismissKeyboard)
                    }
            }
            // Keep this binding bidirectional so native user positions are not
            // replaced by a stale target under the top initial anchor.
            // Tail navigation uses one-shot ScrollViewReader commands below.
            .scrollPosition($position)
            .environment(\.streamingGlyphAnimationPaused, scrolling.userIsScrolling)
            .scrollDismissesKeyboard(.interactively)
            .scrollIndicators(.hidden)
            .scrollBounceBehavior(.always, axes: .vertical)
            .scrollEdgeEffectStyle(.soft, for: .top)
            .defaultScrollAnchor(.top, for: .initialOffset)
            .defaultScrollAnchor(.bottom, for: .alignment)
            // Short timelines still belong at the newest message if SwiftUI
            // resolves an unset position after the parent view is updated.
            .allowsHitTesting(model.isOpeningReady)
            .accessibilityHidden(!model.isOpeningReady)
            // A tap in the transcript dismisses the keyboard. It is installed
            // on the underlying scroll view (see ChatTranscriptTapDismiss):
            // a SwiftUI tap gesture here lost to message text's selection
            // overlay, so tapping the upper half of the screen never closed
            // the keyboard. Dragging still uses .scrollDismissesKeyboard.
            .onScrollPhaseChange { _, phase, context in
                // A phase callback carries a newer authoritative sample.
                viewportUpdates.cancel()
                let mapped: TimelineScrollState.Phase
                switch phase {
                case .idle: mapped = .idle
                case .tracking: mapped = .tracking
                case .interacting: mapped = .interacting
                case .decelerating: mapped = .decelerating
                case .animating: mapped = .animating
                @unknown default: mapped = .idle
                }
                let current = TimelineViewport(geometry: context.geometry)
                // Enough to read back what the scroll view actually reported
                // around a keyboard transition (the reported visible rect and
                // the real insets disagreed once already).
                let geometry = context.geometry
                TimelineTrace.shared.record("GEOM",
                    "m=\(mapped) con=\(Int(geometry.containerSize.height)) top=\(Int(geometry.contentInsets.top)) bot=\(Int(geometry.contentInsets.bottom)) vis=\(Int(geometry.visibleRect.height)) vmin=\(Int(geometry.visibleRect.minY)) off=\(Int(geometry.contentOffset.y))")
                let wasUserScrolling = scrolling.userIsScrolling
                // Capture growth before publishing this sample: the coalesced
                // geometry action below compares against scrolling.viewport,
                // so growth observed here would otherwise be swallowed and the
                // last tokens of a reply never followed.
                let grew = abs(current.contentHeight - scrolling.viewport.contentHeight) > 0.5
                viewportSample.value = current
                scrolling.geometryChanged(current)
                nativePhase = mapped
                // The drawer owns horizontal navigation. Do not interpret its
                // interrupted scroll callbacks as a fresh vertical reading intent.
                if navigationIsSuspended { return }
                if scrolling.phaseChanged(mapped, viewport: current) {
                    // Release a history point restoration as soon as the user
                    // takes over, including interrupted animations. Measurements
                    // queued by that restoration would otherwise be applied on
                    // the next main-queue turn, after the gesture had begun.
                    releaseScrollPosition()
                    historyUpdates.cancel()
                    // The correction itself is not cancelled: a page that
                    // lands mid-gesture still moved the content under the
                    // reader, and the anchor has to shift it back once the
                    // finger is gone.
                    olderPull.begin(at: current, promptVisible: olderPromptVisible,
                        canLoad: model.session.hasOlderItems && !model.session.isLoadingHistory && olderLoadRequest == nil && latestLoadRequest == nil)
                    latestPull.begin(at: current, promptVisible: latestPromptVisible,
                        canLoad: model.session.hasNewerItems && !model.session.isLoadingHistory && latestLoadRequest == nil && olderLoadRequest == nil)
                }
                if mapped == .interacting { latestPull.update(current); olderPull.update(current) }
                if mapped == .idle || mapped == .decelerating {
                    let shouldLoadLatest = latestPull.end(), shouldLoadOlder = olderPull.end()
                    if shouldLoadOlder { pendingOlderLoad = true }
                    if shouldLoadLatest { pendingLatestLoad = true }
                } else if mapped == .animating { latestPull.cancel(); olderPull.cancel() }
                if mapped == .idle {
                    // Wait for the release bounce to settle before the page is
                    // inserted; requesting on .decelerating put a page into a
                    // moving scroll view and the anchor correction ran after
                    // the reader had already seen the shift.
                    if pendingOlderLoad {
                        pendingOlderLoad = false
                        loadOlder()
                    } else if pendingLatestLoad {
                        pendingLatestLoad = false
                        loadLatest()
                    }
                }
                if mapped == .idle && wasUserScrolling {
                    // A history page can finish laying out while native
                    // deceleration still owns the offset. Its measurements
                    // are intentionally ignored then; replay the cached row
                    // anchor after the gesture settles so the prepend cannot
                    // leave the reader at a shifted point.
                    if let layout = historyLayout.value { applyHistoryLayout(layout) }
                }
                if grew, scrolling.mode == .following, !scrolling.userIsScrolling {
                    scrolling.requestBottom()
                }
            }
            .onScrollGeometryChange(for: TimelineViewport.self) { geometry in
                TimelineViewport(geometry: geometry)
            } action: { previous, value in
                TimelineTrace.shared.offsetSample(previous: previous, value: value,
                    phase: scrolling.phase, mode: scrolling.mode,
                    rows: "\(model.timeline.rows.count):\(model.timeline.rows.first?.id.prefix(8) ?? "-")…\(model.timeline.rows.last?.id.prefix(8) ?? "-") act=\(model.timeline.subagentActivity?.id.prefix(8) ?? "-")")
                scrolling.noteUpwardMovement(from: previous, to: value)
                viewportUpdates.submit(value) { value in
                    // Offset samples are needed for restoration, but do not change
                    // the rendered page. Publish only dimensions used by following.
                    viewportSample.value = value
                    let previous = scrolling.viewport
                    let contentHeightChanged = abs(previous.contentHeight - value.contentHeight) > 0.5
                    let visibleHeightChanged = abs(previous.visibleHeight - value.visibleHeight) > 0.5
                    if contentHeightChanged || previous.visibleHeight != value.visibleHeight
                        || previous.topInset != value.topInset {
                        scrolling.geometryChanged(value)
                    }
                    // Follow content growth with the animated return; an
                    // inset-only change (keyboard, composer, panel) pins the
                    // tail without animation, one instant write per frame —
                    // nothing animates, so nothing can fight. A native
                    // .sizeChanges anchor was tried here and fought the
                    // scrollPosition binding in a ±313 pt loop on device.
                    if scrolling.mayFollowNewContent {
                        if contentHeightChanged {
                            scrolling.requestBottom()
                        } else {
                            scrolling.requestBottom(animated: false)
                        }
                    }
                    if !navigationIsSuspended && scrolling.phase == .interacting {
                        var latest = latestPull, older = olderPull
                        latest.update(value); older.update(value)
                        if latest != latestPull { latestPull = latest }
                        if older != olderPull { olderPull = older }
                    }
                }
            }
            .onChange(of: navigationIsSuspended, initial: true) { _, suspended in
                viewportUpdates.cancel()
                scrolling.setNavigationSuspended(suspended)
                if suspended {
                    releaseScrollPosition()
                    latestPull.cancel(); olderPull.cancel()
                } else {
                    scrolling.phaseChanged(nativePhase, viewport: viewport)
                    if let layout = historyLayout.value { historyDidLayOut(layout) }
                }
            }
            .onChange(of: model.timeline.pendingMessages.last?.id) { _, id in
                // The presentation buffer, not the session's optimistic list,
                // is what the plain stack renders. Waiting for this id means
                // the return is measured with the new bubble already in the
                // layout; requesting it on `session.pendingMessages` (or at
                // send start) could scroll to the previous bottom and leave the
                // bubble one row below the viewport.
                //
                // This one is not gated on following: sending is an explicit
                // request to see what was just sent, even for a reader who had
                // scrolled up. requestBottom still refuses mid-gesture.
                guard model.isOpeningReady, id != nil else { return }
                scrolling.requestBottom()
            }
            .onChange(of: model.timeline.rows.last?.id) { oldID, newID in
                // Incoming terminal messages can append a row without changing
                // the user's optimistic-send state. Follow that explicit event
                // while parked at the live tail; geometry remains responsible
                // for later height changes within the same streaming row.
                guard model.isOpeningReady, newID != nil, newID != oldID,
                      scrolling.mayFollowNewContent else { return }
                scrolling.requestBottom()
            }
            .onChange(of: model.responseRevision) { _, _ in
                // An approval response belongs to the current turn. Follow it
                // only when the reader is still parked at the live tail and no
                // drag or its settlement is in progress; otherwise their
                // reading position wins.
                if model.isOpeningReady, scrolling.mayFollowNewContent {
                    scrolling.requestBottom()
                }
            }
            .onChange(of: hasInteractions, initial: true) { _, presented in
                scrolling.setInteractionPresented(presented)
            }
            .onChange(of: model.isOpeningReady, initial: true) { _, ready in
                if ready { scrolling.open(interactionPresented: hasInteractions) }
            }
            .onChange(of: scrolling.navigationGeneration) { _, _ in
                releaseScrollPosition()
            }
            .task(id: scrolling.pendingBottomRequest) {
                guard let request = scrolling.pendingBottomRequest, !navigationIsSuspended else { return }
                // Coalesce animated returns so scrolling through the same
                // layout cannot restart the animation on every offset sample.
                // A non-animated inset pin runs immediately: it must land in
                // the same frame as the inset change it tracks.
                if request.animated {
                    do { try await Task.sleep(for: .milliseconds(8)) } catch { return }
                }
                guard !Task.isCancelled, !navigationIsSuspended, let command = scrolling.begin(request) else { return }
                scrollToBottom(command, using: scrollProxy)
            }
            .task(id: UserScrollSettlement(needed: scrolling.needsUserScrollSettlement,
                tail: scrolling.tail, generation: scrolling.navigationGeneration)) {
                guard scrolling.needsUserScrollSettlement else { return }
                // Reconcile visibility after the native phase callback. This
                // never holds opening behind a spinner or repeats a scroll.
                do { try await Task.sleep(for: .milliseconds(64)) } catch { return }
                guard !Task.isCancelled else { return }
                scrolling.settleUserScroll()
                if scrolling.mode == .following, !scrolling.atContentBottom {
                    // The settlement can land before the keyboard or the
                    // composer inset finishes moving; pin once it is idle.
                    scrolling.requestBottom(animated: false)
                }
            }
            .task(id: olderLoadRequest) {
                guard let request = olderLoadRequest else { return }
                await model.session.loadOlder()
                guard !Task.isCancelled else { return }
                if historyPosition?.id != request || scrolling.navigationGeneration != request {
                    // The person navigated while the page was in flight (an
                    // interaction card appeared, or they tapped the bottom
                    // pill). The repository still applied the page. Leaving
                    // historyPosition and olderLoadRequest set wedged the
                    // spinner and refused every later older-page load until
                    // the whole session view was recreated.
                    if olderLoadRequest == request { olderLoadRequest = nil }
                    if historyPosition?.id == request { historyPosition = nil }
                    return
                }
                if !model.session.isValid { historyPosition = nil; olderLoadRequest = nil; return }
                // Read the repository projection, not the presentation buffer:
                // the 4 Hz flush can lag the HTTP response, and using the old
                // first row would finish restoration before the page appears.
                let firstRowID = model.repository.cached(sessionId: model.session.id)
                    .flatMap { data in data.items.first { $0.isVisibleInChat }?.id }
                let expectedFirstRowID = firstRowID
                    ?? model.session.timeline.first { $0.value.isVisibleInChat }?.id
                guard !Task.isCancelled, historyPosition?.id == request,
                      scrolling.navigationGeneration == request else { return }
                historyPosition?.receivedPage(firstRowID: expectedFirstRowID)
                // Restore from the visible anchor's new content-space frame
                // when SwiftUI has laid that row out again.
                do { try await Task.sleep(for: .milliseconds(24)) } catch { return }
                guard !Task.isCancelled, historyPosition?.id == request,
                      scrolling.navigationGeneration == request else { return }
                if let origin = historyPosition?.origin,
                   expectedFirstRowID != origin.firstRowID {
                    if let layout = historyLayout.value,
                       layout.anchorRowID == origin.anchorRowID,
                       layout.firstRowID == expectedFirstRowID {
                        applyHistoryLayout(layout)
                    }
                }
            }
            .task(id: HistorySettlement(id: historyPosition?.id,
                ready: historyPosition?.pageArrived == true,
                offset: historyPosition?.restoredOffset, generation: scrolling.navigationGeneration)) {
                guard let request = historyPosition?.id else { return }
                guard historyPosition?.pageArrived == true else { return }
                // Let the point correction reach native layout before ending
                // the spinner. The position itself stays alive: a long message
                // above the anchor can finish parsing later, and that height
                // still has to be taken out of the reader's position.
                do { try await Task.sleep(for: .milliseconds(64)) } catch { return }
                guard !Task.isCancelled, historyPosition?.id == request else { return }
                if historyPosition?.restoredOffset != nil {
                    position.isPositionedByUser = true
                }
                olderLoadRequest = nil
            }
            .task(id: latestLoadRequest) {
                guard let generation = latestLoadRequest else { return }
                await model.session.loadLatest()
                guard !Task.isCancelled else { return }
                // A drag during the fetch must not be undone when it finishes.
                // requestBottom() additionally refuses while that drag is
                // still tracking or decelerating.
                if scrolling.navigationGeneration == generation, !scrolling.userIsScrolling {
                    scrolling.requestBottom()
                }
                latestLoadRequest = nil
            }
            if scrolling.showsBottomButton() {
                Button {
                    latestPull.cancel(); olderPull.cancel()
                    scrolling.requestBottom()
                } label: {
                    Label(String(localized: "到底部"), appSymbol: "arrow.down").font(.caption.weight(.medium)).foregroundStyle(.primary)
                        .padding(.horizontal, 12).frame(height: returnPillHeight)
                        .glassEffect(.regular.interactive(), in: .capsule)
                        .frame(minHeight: 44).contentShape(Rectangle())
                }
                .buttonStyle(.plain).accessibilityIdentifier("chat.timeline.bottom")
                .padding(.bottom, 2)
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .onDisappear { viewportUpdates.cancel(); historyUpdates.cancel() }
        .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillShowNotification)) { _ in
            // Typing is a reason to bring the tail back only for a reader who
            // is still there. Someone reading history keeps their position.
            // The pin itself is not animated: the keyboard already animates,
            // and per-frame instant pins track it exactly.
            guard scrolling.mode == .following || scrolling.atContentBottom else { return }
            scrolling.requestBottom(animated: false)
        }
        .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardDidShowNotification)) { _ in
            // One settled, non-animated pin against the final height.
            if scrolling.mode == .following, !scrolling.atContentBottom {
                scrolling.requestBottom(animated: false)
            }
        }
    }
    private func dismissKeyboard() {
        UIApplication.shared.sendAction(#selector(UIResponder.resignFirstResponder), to: nil, from: nil, for: nil)
    }

    /// Keeps the rows just above the reader parsed before they are realized.
    ///
    /// A row that enters a lazy stack above the viewport at zero height and
    /// then inflates pushes the visible content down, which is the jump people
    /// see when swiping up through history. Parsing ahead of the gesture makes
    /// the row arrive at its final height instead.
    private func prewarmAroundReader() {
        let rows = model.timeline.rows
        guard !rows.isEmpty,
              let topID = visibleHistoryRows.topmostVisibleRowID(),
              let index = rows.firstIndex(where: { $0.id == topID }) else { return }
        guard abs(index - prewarmCursor.index) >= 4 else { return }
        prewarmCursor.index = index
        let lower = max(0, index - 100)
        let upper = min(rows.count, index + 16)
        ChatMarkdownView.prewarm(rows[lower..<upper].map(\.text))
    }
    private func loadOlder() {
        guard model.session.isValid, model.session.hasOlderItems,
              !model.session.isLoadingHistory, olderLoadRequest == nil, latestLoadRequest == nil else { return }
        olderPull.cancel(); latestPull.cancel()
        scrolling.browseHistory()
        position.isPositionedByUser = true
        let visibleRowIDs = Set(model.timeline.rows.map(\.id))
        // The visible set can be empty for a frame (visibility callbacks have
        // not arrived yet); every realized row is measured, so fall back to the
        // topmost retained measurement instead of losing the correction.
        let layout = visibleHistoryRows.anchor(firstRowID: model.timeline.rows.first?.id, rowIDs: visibleRowIDs,
            visibleTop: viewport.isMeasured ? viewport.visibleBottom - viewport.visibleHeight : nil,
            visibleBottom: viewport.isMeasured ? viewport.visibleBottom : nil)
            ?? visibleHistoryRows.measuredAnchor(firstRowID: model.timeline.rows.first?.id, rowIDs: visibleRowIDs)
        historyLayout.value = layout
        TimelineTrace.shared.record("LOAD.OLDER",
            "offset=\(Int(viewport.offsetY)) rows=\(model.timeline.rows.count) anchor=\(layout == nil ? 0 : 1)")
        historyPosition = TimelineHistoryPosition(id: scrolling.navigationGeneration, layout: layout,
            offsetY: viewport.offsetY, topInset: viewport.topInset,
            contentHeight: viewport.contentHeight > 0 ? viewport.contentHeight : nil,
            originFirstRowID: model.timeline.rows.first?.id,
            originLastRowID: model.timeline.rows.last?.id)
        hasRequestedOlder = true
        olderLoadRequest = scrolling.navigationGeneration
    }
    private func loadLatest() {
        guard model.session.isValid, model.session.hasNewerItems,
              !model.session.isLoadingHistory, latestLoadRequest == nil, olderLoadRequest == nil else { return }
        olderPull.cancel(); latestPull.cancel()
        // This is reached from the pull gesture while the offset is still
        // decelerating. Do not request the tail here: the page lands first,
        // and the fetch completion below returns only if the reader has not
        // taken over the offset in the meantime.
        latestLoadRequest = scrolling.navigationGeneration
    }
    private func scrollToBottom(_ command: TimelineScrollState.BottomCommand, using proxy: ScrollViewProxy) {
        // Use smooth animation even during streaming to avoid jarring jumps
        // Reduce animation duration during streaming for more responsive feel
        let streaming = model.isRunning
        let animation: Animation? = command.request.animated
            ? (reduceMotion ? nil :
                (streaming ? .easeOut(duration: 0.15) : .interactiveSpring(response: 0.28, dampingFraction: 1, blendDuration: 0.12)))
            : nil

        TimelineTrace.shared.record("RETURN", "h=\(Int(command.request.contentHeight)) g=\(command.request.generation)")
        withAnimation(animation, completionCriteria: .removed) {
            // A proxy command targets the tail once. A persistent edge target
            // can compete with the user's next drag or with history restoration.
            proxy.scrollTo("tail", anchor: .bottom)
        } completion: {
            let landed = scrolling.complete(command)
            TimelineTrace.shared.record("RETURN.done", "landed=\(landed)")
            if landed { releaseScrollPosition() }
        }
    }
    /// Stops a history point restoration from overriding a new scroll gesture.
    private func releaseScrollPosition() {
        guard position.point != nil else { return }
        var transaction = Transaction(animation: nil)
        transaction.disablesAnimations = true
        withTransaction(transaction) { position.isPositionedByUser = true }
    }
    private func historyDidLayOut(_ layout: TimelineHistoryLayout) {
        // Record it synchronously as well: a touch in the same main-queue turn
        // cancels the coalesced apply, and the idle replay would then restore
        // the pre-page layout and let the inserted page shift the viewport.
        historyLayout.value = layout
        // Apply immediately when the page has arrived. The coalesced hop ran
        // one main-queue turn later, so the shifted content rendered for a
        // frame before the offset was corrected — the visible jump.
        if historyPosition?.pageArrived == true {
            applyHistoryLayout(layout)
        }
        historyUpdates.submit(layout) { layout in
            applyHistoryLayout(layout)
        }
    }

    private func applyHistoryLayout(_ layout: TimelineHistoryLayout) {
        historyLayout.value = layout
        // A finger on the screen always wins. Inertia does not: three pages
        // loaded during one pull were inserted while the view was still
        // decelerating, every correction was skipped, and the reader got one
        // huge offset jump when the scroll finally went idle.
        guard !navigationIsSuspended, !scrolling.fingerIsDown,
              var restoration = historyPosition else { return }
        let offset = restoration.laidOut(layout, currentOffset: viewport.offsetY,
                                         generation: scrolling.navigationGeneration)
        if historyPosition != restoration { historyPosition = restoration }
        guard let offset else { return }
        TimelineTrace.shared.record("RESTORE.anchor", "y=\(Int(offset)) cur=\(Int(viewport.offsetY))")
        var transaction = Transaction(animation: nil)
        transaction.disablesAnimations = true
        withTransaction(transaction) { position.scrollTo(y: offset) }
    }

    private struct HistorySettlement: Equatable {
        let id: Int?
        let ready: Bool
        let offset: CGFloat?
        let generation: Int
    }
    private struct UserScrollSettlement: Equatable {
        let needed: Bool
        let tail: TimelineTailVisibility
        let generation: Int
    }
}

/// Keep the latest measurement without publishing View state inside a layout
/// callback. One main-queue delivery handles a burst; removal invalidates it.
@MainActor private final class ChatLayoutUpdate<Value> {
    private var pending: (() -> Void)?
    private var scheduled = false
    private var generation = 0

    func submit(_ value: Value, apply: @escaping (Value) -> Void) {
        pending = { apply(value) }
        guard !scheduled else { return }
        scheduled = true
        let token = generation
        DispatchQueue.main.async { [weak self] in
            guard let self, self.generation == token else { return }
            let update = self.pending
            self.pending = nil
            self.scheduled = false
            update?()
        }
    }

    func cancel() {
        generation &+= 1
        pending = nil
        scheduled = false
    }
}

private struct ChatTimelineContent: View, Equatable {
    let model: SessionChatModel
    let onAttachment: (V2AttachmentContent) -> Void
    let onFile: (String) -> Void
    let latestPullReady: Bool
    let isLoadingLatest: Bool
    let olderPullReady: Bool
    let isLoadingOlder: Bool
    let keepsOlderPrompt: Bool
    let historyAnchor: TimelineHistoryLayout?
    let timelineRevision: Int
    let notices: [SessionNoticeModel]
    /// Snapshot of id/revision/visibility captured when this value was built.
    let noticeSignature: [String]
    let isRunning: Bool
    let hasOlderItems: Bool
    let hasNewerItems: Bool
    let hasPendingUserMessage: Bool
    let subagentActivity: SubagentActivityPresentation?
    let onLoadOlder: () -> Void
    let onLoadLatest: () -> Void
    let onHistoryRowLayout: (String, TimelineHistoryLayout, TimelineHistoryRowFrame) -> Void
    let onHistoryRowVisibility: (String, Bool) -> Void
    let onPromptVisibility: (Bool) -> Void
    let onOlderPromptVisibility: (Bool) -> Void
    let onTailVisibility: (TimelineTailVisibility.Region, Bool) -> Void
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.model === rhs.model && lhs.latestPullReady == rhs.latestPullReady && lhs.isLoadingLatest == rhs.isLoadingLatest
            && lhs.olderPullReady == rhs.olderPullReady && lhs.isLoadingOlder == rhs.isLoadingOlder
            && lhs.keepsOlderPrompt == rhs.keepsOlderPrompt && lhs.historyAnchor == rhs.historyAnchor
            && lhs.timelineRevision == rhs.timelineRevision
            && lhs.noticeSignature == rhs.noticeSignature && lhs.isRunning == rhs.isRunning
            && lhs.hasOlderItems == rhs.hasOlderItems && lhs.hasNewerItems == rhs.hasNewerItems
            && lhs.hasPendingUserMessage == rhs.hasPendingUserMessage
            && lhs.subagentActivity == rhs.subagentActivity
    }
    var body: some View {
        let visibleNotices = notices.filter { $0.isVisible && !$0.blocks(model.session.id) }
        let groups = TimelineGrouping.groups(model.timeline.rows, interactionTargets: Set(notices
            .filter(\.isVisible).compactMap(\.timelineTargetID)))
        // Resolve notice targets once per body instead of once per row.
        let rowIDs = Set(model.timeline.rows.map(\.id))
        let noticesByTarget = Dictionary(grouping: visibleNotices.filter { $0.timelineTargetID != nil }) { $0.timelineTargetID! }
        let actions = TimelineTurnActions.build(groups: groups, suppressLatest: isRunning || hasNewerItems,
            hasPendingUserMessage: !hasNewerItems && hasPendingUserMessage)
        // Cache content-space frames without publishing SwiftUI state. Visibility
        // callbacks select the actual row under the reader when history loads.
        let anchorGroup = historyAnchor.flatMap { anchor in groups.first { $0.rows.contains { $0.id == anchor.anchorRowID } } }
        // An ordinary stack, deliberately: a lazy column estimates the height
        // of the rows it has not realized, and every one of those estimates is
        // wrong the moment the row is realized. Content height then swings by
        // hundreds of points while the reader scrolls (the on-device trace
        // showed ±1000pt per sample), which moves the content they are reading
        // and makes the history restore compute a target from a guess. With
        // every row laid out for real, the geometry the follow and restore
        // paths read is exact. NATIVE_CHAT.md records this as the original
        // design; the lazy column was the regression.
        VStack(alignment: .leading, spacing: 20) {
            if hasOlderItems || keepsOlderPrompt {
                Group {
                    if isLoadingOlder {
                        HStack(spacing: 8) {
                            ProgressView().progressViewStyle(.circular).controlSize(.small)
                            Text(String(localized: "正在加载较早的消息…"))
                        }.accessibilityElement(children: .combine)
                    } else if hasOlderItems {
                        Button(action: onLoadOlder) {
                            Text(olderPullReady ? String(localized: "松开加载较早的消息") : String(localized: "加载较早的消息"))
                                .frame(maxWidth: .infinity, minHeight: 44)
                                .contentShape(Rectangle())
                        }.disabled(model.session.isLoadingHistory || isLoadingLatest)
                    } else {
                        // Retain the prompt's footprint on the final page; removing
                        // it after restoring would move the reader by another row.
                        Text(String(localized: "已到达会话开头")).foregroundStyle(.secondary)
                    }
                }
                .font(.footnote).frame(maxWidth: .infinity, minHeight: 44)
                .onScrollVisibilityChange(threshold: 0.9) { onOlderPromptVisibility($0) }
            }
            ForEach(groups) { group in
                SessionTimelineGroupView(group: group, chat: model, onAttachment: onAttachment, onFile: onFile,
                    turnAction: actions[group.id],
                    inlineNotices: group.rows.flatMap { noticesByTarget[$0.id] ?? [] })
                    .equatable()
                    .id(group.id)
                    .background {
                        if let firstRowID = model.timeline.rows.first?.id {
                            Color.clear.onGeometryChange(for: TimelineHistoryRowFrame.self) { geometry in
                                let frame = geometry.frame(in: .named("chat.timeline.content"))
                                return TimelineHistoryRowFrame(minY: frame.minY, maxY: frame.maxY)
                            } action: { frame in
                                let isRestorationAnchor = historyAnchor != nil && group.id == anchorGroup?.id
                                let edge: TimelineHistoryLayout.Edge
                                if isRestorationAnchor, let historyAnchor { edge = historyAnchor.edge }
                                else { edge = historyAnchorEdge(for: group, preserving: false) }
                                let layout = TimelineHistoryLayout(firstRowID: firstRowID,
                                    anchorRowID: isRestorationAnchor ? historyAnchor?.anchorRowID ?? group.id : group.id,
                                    edge: edge, y: edge == .top ? frame.minY : frame.maxY)
                                onHistoryRowLayout(group.id, layout, frame)
                            }
                        }
                    }
                    .onScrollVisibilityChange(threshold: 0.01) { onHistoryRowVisibility(group.id, $0) }
            }
            if let activity = subagentActivity {
                // One extra lazy row in the normal transcript flow. The active
                // turn's subagents stay above notices and the next pending
                // message instead of becoming a separate panel.
                SubagentActivityRow(activity: activity)
                    .id(activity.id)
            }
            ForEach(visibleNotices.filter { notice in
                notice.timelineTargetID.map { !rowIDs.contains($0) } ?? true
            }) { item in
                SessionInteractionCard(item: item, chat: model).id(item.id)
                    .transition(.opacity.combined(with: .move(edge: .bottom)))
            }
            ForEach(model.timeline.pendingMessages) { pending in
                PendingMessageRow(pending: pending, chat: model, onAttachment: onAttachment,
                    onDismiss: { model.session.dismissPendingMessage(id: pending.id) })
                    .id(pending.id)
            }
            if hasNewerItems {
                Button(action: onLoadLatest) {
                    Group {
                        if isLoadingLatest { ProgressView(String(localized: "正在加载更新的记录…")) }
                        else { Text(latestPullReady ? String(localized: "松开加载更新的记录") : String(localized: "继续上拉加载更新的记录")) }
                    }.font(.footnote).frame(maxWidth: .infinity, minHeight: 44)
                }
                .disabled(model.session.isLoadingHistory || isLoadingLatest || isLoadingOlder)
                .onScrollVisibilityChange { onPromptVisibility($0) }
            }
            // Constant breathing room under the last row. Session progress is
            // reported by the header, not drawn inside the transcript: a
            // status line here scrolled with the content and ended up half
            // hidden under the composer.
            Color.clear.frame(height: 32)
                .overlay(alignment: .bottom) {
                    Color.clear.frame(height: 96)
                        .onScrollVisibilityChange(threshold: 0.01) { onTailVisibility(.near, $0) }
                        .allowsHitTesting(false).accessibilityHidden(true)
                }
                .overlay(alignment: .bottom) {
                    // Twelve points, not two: the marker decides whether new
                    // content may move the page, and a sliver at the very end
                    // of the content misses small drags often enough to leave
                    // its last "visible" answer in place after the reader has
                    // already scrolled up.
                    Color.clear.frame(height: 12)
                        .onScrollVisibilityChange(threshold: 0.5) { onTailVisibility(.end, $0) }
                        .allowsHitTesting(false).accessibilityHidden(true)
                }
                .id("tail")
        }
        // Prepending a page is a structural correction, not an animation. The
        // explicit point restore below runs in a transaction with animations
        // disabled; this also keeps the inserted rows from cross-fading.
        .animation(nil, value: model.timeline.historyInsertionRevision)
        .animation(reduceMotion ? nil : .smooth(duration: 0.24), value: visibleNotices.map(\.id))
        .modifier(ChatPageContentColumn(horizontalInset: nil))
        .coordinateSpace(name: "chat.timeline.content")
    }

    /// The top edge is stable as tool output or a turn footer grows below it.
    /// The exception is the group that starts the document: older rows of the
    /// same kind (tools, agents, reconnects) are prepended *into* that group,
    /// so its first row and top edge stay put while the rows below move down.
    /// Its trailing edge is the one that follows the inserted page, exactly
    /// like the pre-lazy grouping logic.
    private func historyAnchorEdge(for anchorGroup: ChatTimelineGroup?, preserving: Bool = true) -> TimelineHistoryLayout.Edge {
        if preserving, let historyAnchor { return historyAnchor.edge }
        guard let anchorGroup,
              anchorGroup.id == model.timeline.rows.first?.id,
              let firstRow = anchorGroup.rows.first,
              firstRow.structure.groupKind != .single else { return .top }
        return .bottom
    }

}

private extension TimelineViewport {
    init(geometry: ScrollGeometry) {
        // The inset formula, not visibleRect: during a keyboard transition the
        // reported visibleRect keeps its pre-keyboard height (con+top+bot,
        // larger than the container), which broke every at-bottom decision.
        // The clamped value is only used for following decisions and traces;
        // keyboard resizes are anchored natively by .sizeChanges, and the
        // tail probe is the authority on what is actually visible.
        self.init(contentHeight: geometry.contentSize.height,
            containerHeight: geometry.containerSize.height,
            topInset: geometry.contentInsets.top,
            bottomInset: geometry.contentInsets.bottom,
            offsetY: geometry.contentOffset.y)
    }
}

/// Native offset storage deliberately does not invalidate the SwiftUI view tree.
@MainActor private final class ChatViewportSample {
    var value: TimelineViewport?
}

/// Scroll geometry and its restoration anchor change while UIKit scrolls.
/// Neither sample should invalidate the chat view tree on every scroll frame.
@MainActor private final class ChatHistoryLayoutSample {
    var value: TimelineHistoryLayout?
}

private nonisolated struct TimelineHistoryRowFrame: Equatable, Sendable {
    let minY: CGFloat
    let maxY: CGFloat
}

/// The row whose markdown prewarm last ran, so a scroll that stays within one
/// window does not restart the parse.
@MainActor private final class ChatPrewarmCursor {
    var index = -1
}

@MainActor private final class ChatVisibleHistoryRows {
    private struct Entry {
        var layout: TimelineHistoryLayout
        var frame: TimelineHistoryRowFrame
    }

    private var entries: [String: Entry] = [:]
    private var visibleIDs: Set<String> = []

    func update(rowID: String, layout: TimelineHistoryLayout, frame: TimelineHistoryRowFrame) {
        entries[rowID] = Entry(layout: layout, frame: frame)
    }

    func setVisible(_ visible: Bool, rowID: String) {
        if visible {
            visibleIDs.insert(rowID)
        } else {
            // Keep the measurement: a content-space frame does not change while
            // the reader scrolls, so a row that comes back into the viewport
            // does not re-emit geometry. Dropping the entry here left the
            // anchor nil after one round trip, and the older page then jumped
            // into view by its full height with no correction at all.
            visibleIDs.remove(rowID)
        }
    }

    /// Forgets rows that no longer exist in the timeline. Called with the live
    /// row ids so the retained measurements stay bounded to the loaded window.
    func prune(rowIDs: Set<String>) {
        guard !entries.isEmpty else { return }
        entries = entries.filter { rowIDs.contains($0.key) }
    }

    /// The row id of the visible group nearest the top of the content, which
    /// is where an upward scroll is heading next.
    func topmostVisibleRowID() -> String? {
        visibleIDs.compactMap { entries[$0] }
            .min { $0.frame.minY < $1.frame.minY }?
            .layout.anchorRowID
    }

    func anchor(firstRowID: String?, rowIDs: Set<String>, visibleTop: CGFloat?, visibleBottom: CGFloat?) -> TimelineHistoryLayout? {
        prune(rowIDs: rowIDs)
        guard let firstRowID else { return nil }
        return visibleIDs.compactMap { entries[$0] }
            .filter { entry in
                entry.layout.firstRowID == firstRowID && rowIDs.contains(entry.layout.anchorRowID)
                    && (visibleTop.map { entry.frame.maxY > $0 } ?? true)
                    && (visibleBottom.map { entry.frame.minY < $0 } ?? true)
            }
            .min { $0.frame.minY < $1.frame.minY }?.layout
    }

    /// The topmost retained measurement for the current page, used when no row
    /// was reported visible yet. The plain stack measures every realized row,
    /// so this is still an exact anchor instead of losing the correction.
    func measuredAnchor(firstRowID: String?, rowIDs: Set<String>) -> TimelineHistoryLayout? {
        guard let firstRowID else { return nil }
        return entries.values
            .filter { $0.layout.firstRowID == firstRowID && rowIDs.contains($0.layout.anchorRowID) }
            .min { $0.frame.minY < $1.frame.minY }?.layout
    }

}

/// A short in-memory record of everything that moves the transcript's offset.
///
/// Static review has proven and fixed the mechanisms it can prove; a jump that
/// survives that needs to be attributed to a writer instead of guessed at.
/// Only suspicious events are kept — an offset change with no gesture, a
/// content-height change, an explicit scroll, a row that inflates after its
/// parse — so the buffer holds minutes of history without recording every
/// frame. "复制滚动诊断" in the session options copies this text.
@MainActor
final class TimelineTrace {
    static let shared = TimelineTrace()

    private struct Event {
        let seq: Int
        let at: TimeInterval
        let kind: String
        let detail: String
    }

    private var events: [Event] = []
    private var nextSeq = 0
    private var lastSampleAt: TimeInterval = 0
    private let limit = 1200

    func record(_ kind: String, _ detail: @autoclosure () -> String = "") {
        let event = Event(seq: nextSeq, at: Date.timeIntervalSinceReferenceDate, kind: kind, detail: detail())
        nextSeq += 1
        events.append(event)
        if events.count > limit { events.removeFirst(events.count - limit) }
    }

    func offsetSample(previous: TimelineViewport, value: TimelineViewport,
                      phase: TimelineScrollState.Phase, mode: TimelineScrollState.Mode, rows: String) {
        let dy = value.offsetY - previous.offsetY
        if abs(dy) > 2, phase == .idle {
            record("JUMP", "dy=\(Int(dy)) offset=\(Int(value.offsetY)) h=\(Int(value.contentHeight)) mode=\(mode)")
        }
        let dh = value.contentHeight - previous.contentHeight
        if abs(dh) > 1 {
            record("HEIGHT", "dh=\(Int(dh)) offset=\(Int(value.offsetY)) phase=\(phase) rows=\(rows)")
        }
        // A quiet heartbeat, so a trace without events still shows what the
        // page was doing around the moment somebody noticed a jump.
        let now = Date.timeIntervalSinceReferenceDate
        if now - lastSampleAt > 3 {
            lastSampleAt = now
            record("SAMPLE", "offset=\(Int(value.offsetY)) h=\(Int(value.contentHeight)) vh=\(Int(value.visibleHeight)) phase=\(phase) mode=\(mode) rows=\(rows)")
        }
    }

    /// The whole buffer, for the on-demand copy.
    func text() -> String {
        guard let first = events.first?.at else { return "no events" }
        return events.map {
            String(format: "%7.1fms ", ($0.at - first) * 1000) + $0.kind + " " + $0.detail
        }.joined(separator: "\n")
    }

    /// Events newer than `after`, with the newest sequence for the next call.
    /// Uploads use this so a periodic report never repeats what was already
    /// sent.
    func text(after after: Int) -> (text: String, last: Int) {
        let fresh = events.filter { $0.seq > after }
        guard let first = fresh.first, let last = fresh.last else { return ("", after) }
        let text = fresh.map {
            String(format: "%7.1fms ", ($0.at - first.at) * 1000) + $0.kind + " " + $0.detail
        }.joined(separator: "\n")
        return (text, last.seq)
    }
}
