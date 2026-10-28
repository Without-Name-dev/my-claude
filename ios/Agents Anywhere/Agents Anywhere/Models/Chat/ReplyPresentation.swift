import Foundation

nonisolated enum ReplyPresentation {
    /// UserDefaults key: true draws new glyphs with a plain opacity fade instead
    /// of the blur-and-rise reveal.
    static let plainFadeKey = "aa.native.chat.reveal.plain-fade"
    static let revealSeconds: TimeInterval = 0.32
    /// Streamed text is revealed in small groups instead of all at once. A
    /// network frame carries a whole sentence or paragraph; fading one batch
    /// as a unit looked like blocks popping in every few hundred milliseconds.
    /// Spreading the glyphs makes the reply read as it is written, and the cap
    /// keeps the text from trailing the network for long.
    static let revealRate: Double = 100
    static let maximumRevealSpread: TimeInterval = 1.0
    static let revealGroupSize = 3
    /// A block's drawing clock must outlive the last scheduled group.
    static var revealClock: TimeInterval { maximumRevealSpread + revealSeconds + drawSlack }
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
