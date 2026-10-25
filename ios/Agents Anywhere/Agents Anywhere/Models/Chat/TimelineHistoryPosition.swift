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
    /// Used when the anchor row is not realized after a lazy prepend. The
    /// inserted page grows the content by exactly the distance the old content
    /// moved down, so the same point correction can be derived from geometry.
    private let originContentHeight: CGFloat?
    private let originFirstRowID: String?
    /// Latest content height seen before the presentation publishes the page.
    /// It keeps streamed tail growth during the request out of the inserted
    /// page's height.
    private var prePublicationContentHeight: CGFloat?
    private var latestLayout: TimelineHistoryLayout?
    private var hasReceivedPage = false
    private var expectedFirstRowID: String?
    private var restorationCancelled = false
    private(set) var restoredOffset: CGFloat?
    private var restoredByHeight = false

    init(id: Int, layout: TimelineHistoryLayout?, offsetY: CGFloat, topInset: CGFloat,
         contentHeight: CGFloat? = nil, originFirstRowID: String? = nil) {
        self.id = id
        origin = layout
        latestLayout = layout
        originContentHeight = contentHeight
        self.originFirstRowID = originFirstRowID ?? layout?.firstRowID
        // An outward pull can still be bouncing when the request starts.
        originOffset = max(-topInset, offsetY)
    }

    var isReadyToFinish: Bool {
        guard hasReceivedPage, let expectedFirstRowID else { return hasReceivedPage }
        // Either the realized anchor reported the page's first row, the lazy
        // prepend was corrected from the content-height delta, or the page did
        // not change the first row at all. Without the middle term, an anchor
        // that left the lazy viewport would keep the spinner (and
        // olderLoadRequest) alive forever after a successful heal; without the
        // last term, an unmeasured unchanged page would do the same.
        return restoredByHeight || expectedFirstRowID == originFirstRowID
            || latestLayout?.firstRowID == expectedFirstRowID
    }

    mutating func receivedPage(firstRowID: String?) {
        hasReceivedPage = true
        expectedFirstRowID = firstRowID
    }

    mutating func cancelRestoration() { restorationCancelled = true }

    mutating func laidOut(_ layout: TimelineHistoryLayout, generation: Int) -> CGFloat? {
        latestLayout = layout
        guard !restorationCancelled, generation == id, let origin,
              layout.anchorRowID == origin.anchorRowID, layout.edge == origin.edge,
              layout.firstRowID != origin.firstRowID else { return nil }
        let offset = originOffset + layout.y - origin.y
        guard abs(offset - (restoredOffset ?? originOffset)) > 0.5 else { return nil }
        restoredOffset = offset
        return offset
    }

    /// Fallback for a prepended page while its anchor row is outside the lazy
    /// viewport. It only runs once the presentation has published the page's
    /// new first row, so streamed tail growth cannot be mistaken for a prepend.
    mutating func laidOutContentHeight(_ height: CGFloat, firstRowID: String?, generation: Int) -> CGFloat? {
        guard !restorationCancelled, !restoredByHeight, generation == id,
              let originContentHeight, originContentHeight > 0 else { return nil }
        guard hasReceivedPage, let expectedFirstRowID else {
            // The page is still in flight. Remember the latest height so
            // streamed tail growth during the request is not added to the
            // inserted page. If the first row already moved, the page itself
            // is laid out; recording this height would erase its own shift.
            if firstRowID == originFirstRowID { prePublicationContentHeight = height }
            return nil
        }
        guard expectedFirstRowID != originFirstRowID else { return nil }
        guard firstRowID == expectedFirstRowID else {
            // The page is in the presentation buffer or behind a reveal batch.
            prePublicationContentHeight = height
            return nil
        }
        // The content-height delta is a fallback for an anchor that the lazy
        // stack dropped. If a realized anchor already reported the new first
        // row, its measured point is exact and this delta would include tail
        // growth, so never let the fallback fight that correction.
        guard latestLayout?.firstRowID != expectedFirstRowID else { return nil }
        let baseline = prePublicationContentHeight ?? originContentHeight
        let offset = originOffset + height - baseline
        guard abs(offset - (restoredOffset ?? originOffset)) > 0.5 else { return nil }
        restoredOffset = offset
        restoredByHeight = true
        return offset
    }
}
