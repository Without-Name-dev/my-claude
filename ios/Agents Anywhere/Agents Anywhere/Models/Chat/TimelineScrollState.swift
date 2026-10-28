import Foundation

/// One owner for opening, following and explicit returns. Native tail visibility
/// decides arrival; geometry only coalesces requests when the layout changes.
nonisolated struct TimelineScrollState: Equatable {
    enum Phase { case idle, tracking, interacting, decelerating, animating }
    enum Mode { case reading, following, returning }

    struct BottomRequest: Equatable {
        let generation: Int
        let contentHeight: CGFloat
        let visibleHeight: CGFloat
    }
    struct BottomCommand: Equatable {
        let id: Int
        let request: BottomRequest
    }

    private(set) var phase = Phase.idle
    private(set) var mode = Mode.reading
    private(set) var viewport = TimelineViewport()
    private(set) var tail = TimelineTailVisibility()
    private(set) var navigationGeneration = 0
    private(set) var interactionIsPresented = false
    private(set) var navigationIsSuspended = false
    private(set) var hasOpened = false
    private(set) var activeCommand: BottomCommand?
    private var lastRequest: BottomRequest?
    private var commandID = 0
    private var awaitsUserScrollSettlement = false
    /// Whether the reader was following the tail when an interaction card
    /// appeared, so dismissing it restores that intent and nothing else.
    private var followedBeforeInteraction = false

    var userIsScrolling: Bool { [.tracking, .interacting, .decelerating].contains(phase) }
    var returningToBottom: Bool { mode == .returning }
    var needsUserScrollSettlement: Bool {
        awaitsUserScrollSettlement && phase == .idle && viewport.isMeasured && !navigationIsSuspended
    }
    /// Whether the reader is parked at the live tail.
    ///
    /// The transcript is a plain stack, so its measured geometry is exact; the
    /// live distance is the only reliable answer. A remembered "bottom offset"
    /// went stale in both directions: streamed tokens pushed the true tail down
    /// (a reader at the bottom was judged to be reading, so the final render of
    /// a reply was never followed), and collapsing a fold pulled it up (a reader
    /// at the bottom was judged to be reading for the rest of the session). The
    /// end marker alone is not usable either — its callback does not fire
    /// reliably for a small clear overlay.
    var atContentBottom: Bool {
        guard viewport.isMeasured else { return tail.isAtBottom }
        return viewport.distanceFromBottom <= Self.bottomTolerance
    }
    /// How far above the live tail still counts as being at it.
    ///
    /// A real upward swipe must leave follow mode. Anything larger turns every
    /// deliberate small scroll into "still at the bottom", so the next streamed
    /// token pulls the reader back down — the ratchet that made the page jump.
    static let bottomTolerance: CGFloat = 8
    /// New content may move the viewport only when the reader is still parked
    /// at the live tail and neither a gesture nor its pending settlement owns
    /// the offset.
    var mayFollowNewContent: Bool {
        !userIsScrolling && !navigationIsSuspended && !needsUserScrollSettlement
            && (mode == .following || atContentBottom)
    }

    mutating func open(interactionPresented: Bool = false) {
        guard !hasOpened else { return }
        hasOpened = true
        interactionIsPresented = interactionPresented
        // Opening is not a return the reader can be in the middle of: the view
        // is not hit-testable until it is ready. A transient phase callback
        // (for example deceleration reported while the first layout lands) must
        // not swallow the one opening return, or the timeline stays in reading
        // mode and never follows the reply that is already streaming. The
        // actual scroll is still gated by pendingBottomRequest.
        armBottomReturn()
    }

    /// The choke point for an explicit return to the tail. A live gesture owns
    /// the offset: callers that may run mid-scroll (sending, an accepted
    /// response, history loading or the pill) must not arm a scroll while the
    /// person is tracking or the offset is decelerating. Refusing here, and
    /// not only at the command site, also prevents a request queued mid-gesture
    /// from firing as soon as the gesture ends.
    @discardableResult
    mutating func requestBottom() -> Bool {
        guard !userIsScrolling else { return false }
        // Keyboard animation and streamed layout changes can request the same
        // return repeatedly. Keep its generation stable while the target is
        // unchanged, but retarget when the layout moved under the animation: a
        // new row or the keyboard inset used to be dropped because the mode was
        // .returning, which left the newest message below the fold.
        if mode == .returning, let lastRequest,
           abs(lastRequest.contentHeight - viewport.contentHeight.rounded()) <= 0.5,
           abs(lastRequest.visibleHeight - viewport.visibleHeight.rounded()) <= 0.5 {
            return true
        }
        armBottomReturn()
        return true
    }

    /// Arms the return without consulting the live phase. Reserved for state
    /// transitions that must restore follow mode (opening, dismissing the last
    /// interaction): they are not competing with a gesture the reader started,
    /// and pendingBottomRequest still refuses to move the viewport until the
    /// scroll view reports idle.
    private mutating func armBottomReturn() {
        mode = .returning
        awaitsUserScrollSettlement = false
        invalidateNavigation()
    }

    mutating func browseHistory() {
        mode = .reading
        awaitsUserScrollSettlement = false
        invalidateNavigation()
    }

    mutating func setInteractionPresented(_ presented: Bool) {
        guard interactionIsPresented != presented else { return }
        interactionIsPresented = presented
        if presented {
            // Remember the reader's intent. Dismissing a card used to arm a
            // return unconditionally, which yanked people who had deliberately
            // scrolled up while an approval was open.
            followedBeforeInteraction = mode == .following || mode == .returning || atContentBottom
            browseHistory()
        } else if followedBeforeInteraction, atContentBottom {
            // The reply resumes as soon as the last card is dismissed. A
            // transient deceleration callback must not leave the timeline in
            // reading mode for the rest of the stream; arm the return directly
            // (the scroll itself still waits for idle in pendingBottomRequest).
            // Someone who scrolled up while the card was open stays put.
            armBottomReturn()
        }
    }

    mutating func setNavigationSuspended(_ suspended: Bool) {
        guard navigationIsSuspended != suspended else { return }
        navigationIsSuspended = suspended
        // Preserve reading/history intent. A hidden or moving drawer cannot
        // retain a native edge target or acknowledge an interrupted animation.
        activeCommand = nil
        lastRequest = nil
    }

    mutating func geometryChanged(_ next: TimelineViewport) { viewport = next }

    /// Claims the offset for the reader the moment it moves upward.
    ///
    /// A geometry sample can arrive a frame before the phase callback. In that
    /// window the timeline still believes it is following, and a follow request
    /// armed by the same frame would scroll a page the person is already
    /// dragging. An upward offset change while idle has no other author: the
    /// return animation and history restoration both move the page down or only
    /// while reading.
    mutating func noteUpwardMovement(from previous: TimelineViewport, to next: TimelineViewport) {
        guard phase == .idle, mode != .reading, !returningToBottom, next.isMeasured else { return }
        guard next.offsetY < previous.offsetY - 2 else { return }
        browseHistory()
    }

    mutating func tailVisibilityChanged(_ region: TimelineTailVisibility.Region, visible: Bool) {
        tail.update(region, visible: visible)
    }

    /// Phase and visibility callbacks can arrive in either order. Keep a manual
    /// drag in reading mode until both have settled, so a stale visible marker
    /// cannot grant auto-follow and pull the reader back down.
    @discardableResult mutating func phaseChanged(_ next: Phase, viewport: TimelineViewport) -> Bool {
        self.viewport = viewport
        guard !navigationIsSuspended else { return false }
        let beganGesture = next == .tracking && phase != .tracking
            || next == .interacting && phase != .tracking && phase != .interacting
        // A busy main thread can deliver one merged callback that lands on
        // .decelerating. The reader still owns the offset then; waiting for a
        // .tracking callback that will never arrive left the timeline following
        // the tail, so the next streamed token pulled the page back down.
        let userPhase = next == .tracking || next == .interacting || next == .decelerating
        if userPhase {
            if mode != .reading { browseHistory() }
            awaitsUserScrollSettlement = true
        }
        phase = next
        return beganGesture
    }

    mutating func settleUserScroll() {
        guard needsUserScrollSettlement, !returningToBottom else { return }
        awaitsUserScrollSettlement = false
        mode = atContentBottom && !interactionIsPresented ? .following : .reading
    }

    var pendingBottomRequest: BottomRequest? {
        // A gesture sample can arrive between requestBottom() and this read.
        // The hard guard applies even to an explicit return: the reader's
        // finger or its inertia must never be fought by a forced scroll.
        guard !userIsScrolling else { return nil }
        guard hasOpened, !navigationIsSuspended, viewport.isMeasured,
              returningToBottom || tail.isMeasured,
              mode != .reading,
              !interactionIsPresented || returningToBottom else { return nil }
        // A return is issued once even for short content. Subsequent layout
        // changes only need correction when they actually move away from bottom.
        if atContentBottom && (mode == .following || lastRequest != nil) { return nil }
        let request = BottomRequest(generation: navigationGeneration,
            contentHeight: viewport.contentHeight.rounded(), visibleHeight: viewport.visibleHeight.rounded())
        return request == lastRequest ? nil : request
    }

    mutating func begin(_ request: BottomRequest) -> BottomCommand? {
        guard pendingBottomRequest == request else { return nil }
        commandID &+= 1
        let command = BottomCommand(id: commandID, request: request)
        lastRequest = request
        activeCommand = command
        return command
    }

    /// Only the latest animation can release ScrollPosition. A new gesture,
    /// approval or drawer transition invalidates an old completion immediately.
    mutating func complete(_ command: BottomCommand) -> Bool {
        guard activeCommand == command else { return false }
        activeCommand = nil
        if returningToBottom {
            mode = interactionIsPresented ? .reading : .following
            // Content that arrived during the animation leaves the viewport
            // short of the tail. Nothing else schedules another scroll — the
            // final render of a reply often lands exactly this way — so
            // re-arm the return instead of stopping one line above the end.
            let landed = viewport.isMeasured
                && abs(viewport.contentHeight - command.request.contentHeight) <= 0.5
                && abs(viewport.visibleHeight - command.request.visibleHeight) <= 0.5
            if !landed, mode == .following, !atContentBottom {
                armBottomReturn()
            }
        }
        return true
    }

    func showsBottomButton() -> Bool {
        // Distance, not the probe: the marker's callbacks are unreliable for a
        // small clear overlay, and a pill that never appears removes the only
        // manual way back to the tail.
        hasOpened && !navigationIsSuspended && phase == .idle
            && !atContentBottom && activeCommand == nil && pendingBottomRequest == nil
    }

    private mutating func invalidateNavigation() {
        navigationGeneration &+= 1
        lastRequest = nil
        activeCommand = nil
    }
}

/// Two probes overlap the existing tail spacer. The larger region provides the
/// return pill's 96-point margin; only the end marker grants automatic following.
nonisolated struct TimelineTailVisibility: Equatable {
    enum Region { case near, end }
    private var near: Bool?
    private var end: Bool?
    var isMeasured: Bool { near != nil && end != nil }
    var isAtBottom: Bool { end == true }
    var isNearBottom: Bool { isAtBottom || near != false }

    mutating func update(_ region: Region, visible: Bool) {
        switch region {
        case .near: near = visible
        case .end: end = visible
        }
    }
}
