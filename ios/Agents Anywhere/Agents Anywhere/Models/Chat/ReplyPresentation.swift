import Foundation

nonisolated enum ReplyPresentation {
    /// UserDefaults key: true draws new glyphs with a plain opacity fade instead
    /// of the blur-and-rise reveal.
    static let plainFadeKey = "aa.native.chat.reveal.plain-fade"
    static let revealSeconds: TimeInterval = 0.4
    /// Streamed text is revealed in small groups instead of all at once. A
    /// network frame carries a whole sentence or paragraph; fading one batch
    /// as a unit looked like blocks popping in every few hundred milliseconds.
    ///
    /// Pacing model: glyphs join a typing queue at `revealRate`. A network
    /// burst (70–140 characters every ~0.5 s) is absorbed as queue lead, so
    /// the visible output is a continuous stream rather than one fade per
    /// frame. The lead is bounded: past `hardRevealLead` the spacing
    /// compresses, so the reveal trails the network by at most a few seconds
    /// no matter how long the reply runs.
    static let revealRate: Double = 75
    static let revealGroupSize = 2
    static let hardRevealLead: TimeInterval = 3.0
    /// A block's drawing clock must outlive the last scheduled group.
    static var revealClock: TimeInterval { hardRevealLead + revealSeconds + drawSlack }
    /// A published batch starts drawing after its Markdown parse and the next
    /// frame. Drawing clocks stay alive this much longer than the reveal.
    static let drawSlack: TimeInterval = 0.1
    /// A completed block keeps its drawing clock until the staggered glyphs
    /// have finished, then switches to native text drawing.
    static let settleDelay: Duration = .seconds(revealClock)
}

/// Advance deadlines independently of the work done at each one, so processing
/// time doesn't accumulate into a slower cadence. Late deadlines skip ahead.
nonisolated struct ReplyFlushSchedule {
    let interval: Duration
    private(set) var deadline: ContinuousClock.Instant

    init(start: ContinuousClock.Instant, interval: Duration) {
        precondition(interval > .zero)
        self.interval = interval
        deadline = start.advanced(by: interval)
    }

    mutating func advance(after now: ContinuousClock.Instant) {
        deadline = deadline.advanced(by: interval)
        if deadline <= now { deadline = now.advanced(by: interval) }
    }
}
