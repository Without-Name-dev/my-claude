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
    @State private var historyHeights = ChatLayoutUpdate<CGFloat>()
    @State private var latestPull = TimelineHistoryPull()
    @State private var olderPull = TimelineHistoryPull(edge: .older)
    @State private var olderPromptVisible = false
    @State private var olderLoadRequest: Int?
    @State private var latestPromptVisible = false
    @State private var latestLoadRequest: Int?
    @State private var nativePhase = TimelineScrollState.Phase.idle
    /// The return pill would sit on top of the keyboard while typing.
    @State private var keyboardIsVisible = false
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
                let wasUserScrolling = scrolling.userIsScrolling
                let wasInteracting = scrolling.phase == .interacting
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
                    historyUpdates.cancel(); historyHeights.cancel()
                    historyPosition?.cancelRestoration()
                    olderPull.begin(at: current, promptVisible: olderPromptVisible,
                        canLoad: model.session.hasOlderItems && !model.session.isLoadingHistory && olderLoadRequest == nil && latestLoadRequest == nil)
                    latestPull.begin(at: current, promptVisible: latestPromptVisible,
                        canLoad: model.session.hasNewerItems && !model.session.isLoadingHistory && latestLoadRequest == nil && olderLoadRequest == nil)
                }
                if mapped == .interacting { latestPull.update(current); olderPull.update(current) }
                if mapped == .idle || mapped == .decelerating {
                    let shouldLoadLatest = latestPull.end(), shouldLoadOlder = olderPull.end()
                    if wasInteracting {
                        if shouldLoadOlder { loadOlder() }
                        else if shouldLoadLatest { loadLatest() }
                    }
                } else if mapped == .animating { latestPull.cancel(); olderPull.cancel() }
                if mapped == .idle && wasUserScrolling {
                    // A history page can finish laying out while native
                    // deceleration still owns the offset. Its measurements
                    // are intentionally ignored then; replay the cached row
                    // anchor after the gesture settles so the prepend cannot
                    // leave the reader at a shifted point.
                    if let layout = historyLayout.value { applyHistoryLayout(layout) }
                    applyHistoryHeight(viewport.contentHeight)
                }
            }
            .onScrollGeometryChange(for: TimelineViewport.self) { geometry in
                TimelineViewport(geometry: geometry)
            } action: { previous, value in
                TimelineTrace.shared.offsetSample(previous: previous, value: value,
                    phase: scrolling.phase, mode: scrolling.mode,
                    rows: "\(model.timeline.rows.count):\(model.timeline.rows.first?.id.prefix(10) ?? "-")")
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
                    // Follow on keyboard show OR streaming content growth (when already following)
                    if scrolling.mayFollowNewContent {
                        if keyboardIsVisible && visibleHeightChanged {
                            scrolling.requestBottom()
                        } else if contentHeightChanged && model.isRunning {
                            // During streaming, follow content growth to show new tokens
                            scrolling.requestBottom()
                        }
                    }
                    if !navigationIsSuspended && scrolling.phase == .interacting {
                        var latest = latestPull, older = olderPull
                        latest.update(value); older.update(value)
                        if latest != latestPull { latestPull = latest }
                        if older != olderPull { olderPull = older }
                    }
                }
                if abs(value.contentHeight - previous.contentHeight) > 0.5 {
                    historyHeights.submit(value.contentHeight) { applyHistoryHeight($0) }
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
                    applyHistoryHeight(viewport.contentHeight)
                }
            }
            .onChange(of: model.timeline.pendingMessages.last?.id) { _, id in
                // The presentation buffer, not the session's optimistic list,
                // is what the LazyVStack renders. Waiting for this id means the
                // return is measured with the new bubble already in the layout;
                // requesting it on `session.pendingMessages` (or at send start)
                // could scroll to the previous bottom and leave the bubble one
                // row below the viewport.
                if model.isOpeningReady, id != nil, scrolling.mayFollowNewContent { scrolling.requestBottom() }
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
                // Coalesce actual layout changes. Scrolling through the same
                // layout cannot restart this animation on every offset callback.
                do { try await Task.sleep(for: .milliseconds(8)) } catch { return }
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
            }
            .task(id: olderLoadRequest) {
                guard let request = olderLoadRequest else { return }
                await model.session.loadOlder()
                guard !Task.isCancelled, historyPosition?.id == request,
                      scrolling.navigationGeneration == request else { return }
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
                applyHistoryHeight(viewport.contentHeight)
            }
            .task(id: HistorySettlement(id: historyPosition?.id,
                ready: historyPosition?.isReadyToFinish() == true,
                offset: historyPosition?.restoredOffset, generation: scrolling.navigationGeneration)) {
                guard let request = historyPosition?.id else { return }
                // A gesture or an explicit return to bottom supersedes the
                // restoration. The page may still arrive through the
                // repository, but it must not hold the older prompt busy.
                if scrolling.navigationGeneration != request {
                    historyPosition = nil
                    olderLoadRequest = nil
                    return
                }
                guard historyPosition?.isReadyToFinish() == true else { return }
                // Let the point correction reach native layout before releasing
                // its target or ending the spinner. A new measurement restarts this.
                do { try await Task.sleep(for: .milliseconds(64)) } catch { return }
                guard !Task.isCancelled, historyPosition?.id == request else { return }
                if historyPosition?.restoredOffset != nil {
                    position.isPositionedByUser = true
                }
                historyPosition = nil; olderLoadRequest = nil
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
            if !keyboardIsVisible, scrolling.showsBottomButton() {
                Button {
                    latestPull.cancel(); olderPull.cancel()
                    historyPosition?.cancelRestoration()
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
        .onDisappear { viewportUpdates.cancel(); historyUpdates.cancel(); historyHeights.cancel() }
        .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillShowNotification)) { _ in
            keyboardIsVisible = true
            // Typing is a reason to bring the tail back only for a reader who
            // is still there. Someone reading history keeps their position.
            guard scrolling.mode == .following || scrolling.atContentBottom else { return }
            // Editing is an explicit request to bring the latest conversation
            // into the unobscured viewport. Geometry updates during the keyboard
            // animation keep the tail aligned after the available height settles.
            scrolling.requestBottom()
        }
        .onReceive(NotificationCenter.default.publisher(for: UIResponder.keyboardWillHideNotification)) { _ in
            keyboardIsVisible = false
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
        let layout = visibleHistoryRows.anchor(firstRowID: model.timeline.rows.first?.id, rowIDs: visibleRowIDs,
            visibleTop: viewport.isMeasured ? viewport.visibleBottom - viewport.visibleHeight : nil,
            visibleBottom: viewport.isMeasured ? viewport.visibleBottom : nil)
        historyLayout.value = layout
        TimelineTrace.shared.record("LOAD.OLDER", "offset=\(Int(viewport.offsetY)) rows=\(model.timeline.rows.count)")
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
        historyPosition?.cancelRestoration()
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
        let animation: Animation? = reduceMotion ? nil :
            (streaming ? .easeOut(duration: 0.15) : .interactiveSpring(response: 0.28, dampingFraction: 1, blendDuration: 0.12))

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
        historyUpdates.submit(layout) { layout in
            applyHistoryLayout(layout)
        }
    }

    private func applyHistoryLayout(_ layout: TimelineHistoryLayout) {
        historyLayout.value = layout
        // Point restoration is a forced scroll too: a live gesture always
        // wins, even if a coalesced measurement arrives after it began.
        guard !navigationIsSuspended, !scrolling.userIsScrolling,
              var restoration = historyPosition else { return }
        let offset = restoration.laidOut(layout, generation: scrolling.navigationGeneration)
        if historyPosition != restoration { historyPosition = restoration }
        guard let offset else { return }
        TimelineTrace.shared.record("RESTORE.anchor", "y=\(Int(offset))")
        var transaction = Transaction(animation: nil)
        transaction.disablesAnimations = true
        withTransaction(transaction) { position.scrollTo(y: offset) }
    }

    private func applyHistoryHeight(_ height: CGFloat) {
        // The height difference between two samples cannot tell "a page was
        // prepended" apart from "the reply at the tail grew". While a reply
        // streams, that fallback would restore by the wrong amount and jump the
        // page; the anchor measurement is the only trustworthy channel then.
        guard !navigationIsSuspended, !scrolling.userIsScrolling, !model.isRunning,
              var restoration = historyPosition else { return }
        let offset = restoration.laidOutContentHeight(height,
            firstRowID: model.timeline.rows.first?.id,
            lastRowID: model.timeline.rows.last?.id,
            generation: scrolling.navigationGeneration)
        if historyPosition != restoration { historyPosition = restoration }
        guard let offset else { return }
        TimelineTrace.shared.record("RESTORE.height", "y=\(Int(offset))")
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

    static func == (lhs: Self, rhs: Self) -> Bool {
        lhs.model === rhs.model && lhs.latestPullReady == rhs.latestPullReady && lhs.isLoadingLatest == rhs.isLoadingLatest
            && lhs.olderPullReady == rhs.olderPullReady && lhs.isLoadingOlder == rhs.isLoadingOlder
            && lhs.keepsOlderPrompt == rhs.keepsOlderPrompt && lhs.historyAnchor == rhs.historyAnchor
            && lhs.timelineRevision == rhs.timelineRevision
            && lhs.notices.map(\.id) == rhs.notices.map(\.id) && lhs.isRunning == rhs.isRunning
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
            }) { item in SessionInteractionCard(item: item, chat: model).id(item.id) }
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
        self.init(contentHeight: geometry.contentSize.height, containerHeight: geometry.containerSize.height,
            topInset: geometry.contentInsets.top, bottomInset: geometry.contentInsets.bottom, offsetY: geometry.contentOffset.y)
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
        if visible { visibleIDs.insert(rowID) }
        else {
            visibleIDs.remove(rowID)
            entries.removeValue(forKey: rowID)
        }
    }

    /// The row id of the visible group nearest the top of the content, which
    /// is where an upward scroll is heading next.
    func topmostVisibleRowID() -> String? {
        visibleIDs.compactMap { entries[$0] }
            .min { $0.frame.minY < $1.frame.minY }?
            .layout.anchorRowID
    }

    func anchor(firstRowID: String?, rowIDs: Set<String>, visibleTop: CGFloat?, visibleBottom: CGFloat?) -> TimelineHistoryLayout? {
        guard let firstRowID else { return nil }
        return visibleIDs.compactMap { entries[$0] }
            .filter { entry in
                entry.layout.firstRowID == firstRowID && rowIDs.contains(entry.layout.anchorRowID)
                    && (visibleTop.map { entry.frame.maxY > $0 } ?? true)
                    && (visibleBottom.map { entry.frame.minY < $0 } ?? true)
            }
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
        let at: TimeInterval
        let kind: String
        let detail: String
    }

    private var events: [Event] = []
    private let limit = 1200

    func record(_ kind: String, _ detail: @autoclosure () -> String = "") {
        let at = Date.timeIntervalSinceReferenceDate
        events.append(Event(at: at, kind: kind, detail: detail()))
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
    }

    func text() -> String {
        guard let first = events.first?.at else { return "no events" }
        return events.map {
            String(format: "%7.1fms ", ($0.at - first) * 1000) + $0.kind + " " + $0.detail
        }.joined(separator: "\n")
    }
}
