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

/// Where the reader was when an older page was requested, and how far that page
/// has moved the content under them since.
///
/// The correction is expressed as an incremental delta against the *current*
/// offset, not as a target computed when the request started: the page can
/// arrive while the person is still dragging, and a point measured then would
/// drag them back. Each time the anchor's content-space position changes by Δ
/// (a page was prepended, or a long message above it finished parsing), the
/// live offset is shifted by the same Δ, which keeps the row they are reading
/// exactly where it is on screen.
///
/// The position stays alive after the page has arrived so those late height
/// changes keep being corrected; a new request or leaving the session replaces
/// it.
nonisolated struct TimelineHistoryPosition: Equatable {
    let id: Int
    let origin: TimelineHistoryLayout?
    private var hasReceivedPage = false
    private var expectedFirstRowID: String?
    private(set) var restoredOffset: CGFloat?
    /// The anchor position the last correction was derived from.
    private var measuredAnchorY: CGFloat
    /// The first row the last accepted correction was derived from. A bottom
    /// edge also moves when its own group grows below the anchor row (a new
    /// tool call in the same folded group); that must not move the reader.
    private var measuredFirstRowID: String?
    private var corrected = false

    init(id: Int, layout: TimelineHistoryLayout?, offsetY: CGFloat, topInset: CGFloat,
         contentHeight: CGFloat? = nil,
         originFirstRowID: String? = nil,
         originLastRowID: String? = nil) {
        self.id = id
        origin = layout
        measuredAnchorY = layout?.y ?? 0
    }

    /// Whether the page itself has arrived. The loading indicator ends here;
    /// the correction may still be waiting for layout or for the reader to stop
    /// dragging.
    var pageArrived: Bool { hasReceivedPage }

    mutating func receivedPage(firstRowID: String?) {
        hasReceivedPage = true
        expectedFirstRowID = firstRowID
    }

    /// Returns the offset the page should be shifted to, or nil when nothing
    /// moved since the last call.
    mutating func laidOut(_ layout: TimelineHistoryLayout, currentOffset: CGFloat, generation: Int) -> CGFloat? {
        guard generation == id, let origin,
              layout.anchorRowID == origin.anchorRowID, layout.edge == origin.edge,
              layout.firstRowID != origin.firstRowID else { return nil }
        if let expectedFirstRowID, layout.firstRowID != expectedFirstRowID { return nil }
        // A bottom edge follows the inserted page, but it also follows a
        // streamed row appended to the same folded group. Only a change of the
        // first row is a page; anything else would yank the reader by the
        // group's growth.
        let reference = measuredFirstRowID ?? origin.firstRowID
        if layout.edge == .bottom, layout.firstRowID == reference { return nil }
        measuredFirstRowID = layout.firstRowID
        let delta = layout.y - measuredAnchorY
        guard abs(delta) > 0.5 else { return nil }
        measuredAnchorY = layout.y
        let offset = currentOffset + delta
        restoredOffset = offset
        corrected = true
        return offset
    }
}
