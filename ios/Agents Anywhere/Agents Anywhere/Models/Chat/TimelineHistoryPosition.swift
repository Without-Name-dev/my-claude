import Foundation

/// One measured, already-rendered group in content coordinates. Measuring the
/// anchor (instead of total content height) excludes simultaneous tail appends.
nonisolated struct TimelineHistoryLayout: Equatable {
    enum Edge { case top, bottom }
    let firstRowID: String
    let anchorRowID: String
    let edge: Edge
    let y: CGFloat
}

/// A history request stays busy until its page has passed through the 5 Hz
/// presentation buffer and layout. Restoring a point retains the reader's
/// offset inside a long message, unlike aligning a group ID to the viewport top.
nonisolated struct TimelineHistoryPosition: Equatable {
    let id: Int
    let origin: TimelineHistoryLayout?
    private let originOffset: CGFloat
    private let originContentHeight: CGFloat?
    private let originFirstRowID: String?
    /// The newest row when the page was requested. Any row appended after that
    /// grew the tail, and the height difference between two samples then mixes
    /// "a page was prepended" with "the reply grew" — the height fallback is
    /// only trustworthy while this still matches.
    private let originLastRowID: String?
    private var prePublicationContentHeight: CGFloat?
    private var hasReceivedPage = false
    private var expectedFirstRowID: String?
    private var restorationCancelled = false
    private(set) var restoredOffset: CGFloat?
    private var restoredByAnchor = false
    private var restoredByHeight = false

    init(id: Int, layout: TimelineHistoryLayout?, offsetY: CGFloat, topInset: CGFloat,
         contentHeight: CGFloat? = nil,
         originFirstRowID: String? = nil,
         originLastRowID: String? = nil) {
        self.id = id
        origin = layout
        originContentHeight = contentHeight
        self.originFirstRowID = originFirstRowID ?? layout?.firstRowID
        self.originLastRowID = originLastRowID
        // An outward pull can still be bouncing when the request starts.
        originOffset = max(-topInset, offsetY)
    }

    func isReadyToFinish() -> Bool {
        guard hasReceivedPage else { return false }
        guard let expectedFirstRowID else { return true }
        return restoredByHeight || restoredByAnchor || expectedFirstRowID == originFirstRowID
    }

    mutating func receivedPage(firstRowID: String?) {
        hasReceivedPage = true
        expectedFirstRowID = firstRowID
    }

    mutating func cancelRestoration() { restorationCancelled = true }

    mutating func laidOut(_ layout: TimelineHistoryLayout, generation: Int) -> CGFloat? {
        // The anchor is the trustworthy channel, so it may correct an earlier
        // height-based guess (that one can be polluted). The reverse is not
        // allowed: see laidOutContentHeight.
        guard !restorationCancelled, generation == id, let origin,
              layout.anchorRowID == origin.anchorRowID, layout.edge == origin.edge,
              layout.firstRowID != origin.firstRowID else { return nil }
        guard expectedFirstRowID == nil || layout.firstRowID == expectedFirstRowID else { return nil }
        restoredByAnchor = true
        let offset = originOffset + layout.y - origin.y
        guard abs(offset - (restoredOffset ?? originOffset)) > 0.5 else { return nil }
        restoredOffset = offset
        return offset
    }

    mutating func laidOutContentHeight(_ height: CGFloat, firstRowID: String?, lastRowID: String?, generation: Int) -> CGFloat? {
        guard !restorationCancelled, !restoredByHeight, generation == id,
              let originContentHeight, originContentHeight > 0 else { return nil }
        // Something was appended at the tail since the page was requested; the
        // height delta now includes that growth, so the fallback would restore
        // by the wrong amount.
        if let originLastRowID, lastRowID != originLastRowID { return nil }
        guard hasReceivedPage, let expectedFirstRowID else {
            if firstRowID == originFirstRowID { prePublicationContentHeight = height }
            return nil
        }
        guard expectedFirstRowID != originFirstRowID else { return nil }
        guard firstRowID == expectedFirstRowID else {
            prePublicationContentHeight = height
            return nil
        }
        guard !restoredByAnchor else { return nil }
        let baseline = prePublicationContentHeight ?? originContentHeight
        let offset = originOffset + height - baseline
        guard abs(offset - (restoredOffset ?? originOffset)) > 0.5 else { return nil }
        restoredOffset = offset
        restoredByHeight = true
        return offset
    }
}
