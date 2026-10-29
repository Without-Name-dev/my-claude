import Foundation

/// One reveal curve drives opacity, blur and translation without changing the
/// text's measured size. Geometry always uses the final, untransformed glyphs.
nonisolated struct GlyphRevealEffect {
    let progress: Double
    /// With the reveal animation switched off, new glyphs only fade in.
    var rises = true
    var opacity: Double { progress }
    var blurRadius: Double { rises ? 3 * (1 - progress) : 0 }
    var offsetY: Double { rises ? 3 * (1 - progress) : 0 }
}

/// Glyphs before `settledCount` draw normally. Each batch is one flush's
/// appended glyphs; they were born together and share one reveal progress.
nonisolated struct GlyphRevealProgress: Equatable {
    struct Batch: Equatable {
        let range: Range<Int>
        let progress: Double
    }

    let settledCount: Int
    let batches: [Batch]

    func value(at index: Int) -> Double {
        batches.first { $0.range.contains(index) }?.progress ?? 1
    }
}

/// TextRenderer may draw off the main actor. Birth times belong to the newly
/// appended glyphs; a later flush never restarts an earlier batch's animation.
nonisolated final class GlyphRevealLedger: @unchecked Sendable {
    private let lock = NSLock()
    private let duration: TimeInterval
    private var settledCount = 0
    // One entry per flush, oldest first. A frame costs O(batches), not
    // O(characters), however long the paragraph grows.
    private var births: [(count: Int, born: TimeInterval)] = []
    /// When the next appended glyph should be born, so batches stay ordered
    /// across flushes instead of interleaving later text with earlier text.
    private var nextBirth: TimeInterval = 0

    init(duration: TimeInterval = ReplyPresentation.revealSeconds) {
        precondition(duration > 0 && duration.isFinite)
        self.duration = duration
    }

    func progress(count: Int, now: TimeInterval, enabled: Bool) -> GlyphRevealProgress? {
        lock.lock()
        defer { lock.unlock() }
        // Textual can briefly emit an empty Text while rebuilding a heading or
        // fragment. That intermediate draw must not erase earlier glyph births.
        guard count > 0 else { return nil }
        guard enabled else {
            settledCount = count
            births.removeAll(keepingCapacity: false)
            nextBirth = now
            return nil
        }
        settledCount = min(settledCount, count)
        let revealingCount = count - settledCount
        var birthCount = births.reduce(0) { $0 + $1.count }
        while birthCount > revealingCount, let last = births.last {
            let removed = min(last.count, birthCount - revealingCount)
            if removed == last.count { births.removeLast() } else { births[births.count - 1].count -= removed }
            birthCount -= removed
        }
        // A replacement (not an append) can shorten the text. Keep the next
        // birth after the remaining queue instead of leaving it in the future.
        if births.isEmpty {
            nextBirth = min(nextBirth, now)
        } else if let lastBorn = births.last?.born, nextBirth > lastBorn + ReplyPresentation.hardRevealLead {
            nextBirth = lastBorn + ReplyPresentation.hardRevealLead
        }
        if revealingCount > birthCount {
            // A flush can append a whole paragraph. One shared birth time made
            // all of it fade in together, which reads as a block popping in.
            // Schedule small groups a few milliseconds apart instead.
            //
            // Births must stay ordered across flushes: scheduling a new chunk
            // from "now" while the previous chunk still has groups queued made
            // later text visible before earlier text. Continue from the end of
            // the existing queue, at the natural typing rate, compressing only
            // once the lead reaches the hard cap so the reveal can never trail
            // the network by more than the bounded reveal lead.
            let added = revealingCount - birthCount
            let start = max(now, nextBirth)
            let lead = start - now
            let spread = min(max(0, ReplyPresentation.hardRevealLead - lead),
                             Double(added) / ReplyPresentation.revealRate)
            // At most ~300 groups keeps the per-frame batch walk bounded even
            // for a large recovery chunk; two glyphs per group at typing rate
            // reads as continuous writing.
            let groups = max(1, min(300, Int((Double(added) / Double(ReplyPresentation.revealGroupSize)).rounded(.up))))
            let size = max(1, Int((Double(added) / Double(groups)).rounded(.up)))
            let interval = groups > 1 ? spread / Double(groups - 1) : 0
            var remaining = added
            var born = start
            while remaining > 0 {
                let step = min(remaining, size)
                births.append((step, born))
                remaining -= step
                born += interval
            }
            nextBirth = start + spread
        }
        // Finished batches join the settled prefix and leave the per-frame work.
        while let first = births.first, now - first.born >= duration {
            settledCount += first.count
            births.removeFirst()
        }
        guard !births.isEmpty else { return nil }
        var start = settledCount
        return GlyphRevealProgress(settledCount: settledCount, batches: births.map { birth in
            defer { start += birth.count }
            let progress = min(1, max(0, (now - birth.born) / duration))
            return .init(range: start..<(start + birth.count), progress: 1 - pow(1 - progress, 3))
        })
    }
}
