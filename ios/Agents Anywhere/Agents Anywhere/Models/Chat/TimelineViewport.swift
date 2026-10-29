import Foundation

/// Relative geometry for detecting layout changes and retaining history. It
/// deliberately does not infer arrival or a target offset from native insets.
nonisolated struct TimelineViewport: Equatable {
    let contentHeight: CGFloat
    let visibleHeight: CGFloat
    let visibleBottom: CGFloat
    let offsetY: CGFloat
    let topInset: CGFloat

    init(contentHeight: CGFloat = 0, containerHeight: CGFloat = 0,
         topInset: CGFloat = 0, bottomInset: CGFloat = 0, offsetY: CGFloat = 0) {
        self.contentHeight = max(0, contentHeight)
        visibleHeight = max(0, containerHeight - topInset - bottomInset)
        visibleBottom = offsetY + containerHeight - bottomInset
        self.offsetY = offsetY
        self.topInset = topInset
        // The container, not the clamped difference: during a keyboard
        // transition the insets can exceed the container for a few frames,
        // and treating the page as unmeasured then disabled every follow
        // decision exactly while typing.
        measured = containerHeight > 0
    }

    private var measured: Bool = false

    /// Builds from explicit parts.
    init(contentHeight: CGFloat, visibleHeight: CGFloat, visibleBottom: CGFloat,
         offsetY: CGFloat, topInset: CGFloat, measured: Bool = true) {
        self.contentHeight = max(0, contentHeight)
        self.visibleHeight = max(0, visibleHeight)
        self.visibleBottom = visibleBottom
        self.offsetY = offsetY
        self.topInset = topInset
        self.measured = measured
    }
    var isMeasured: Bool { measured }
    /// Position of the visible bottom relative to the scroll offset. Unlike
    /// visibleHeight, this does not collapse to zero during inset transitions.
    var bottomEdge: CGFloat { visibleBottom - offsetY }
    /// How far the bottom edge of the viewport sits above the end of the
    /// content. Zero when the reader is at the tail or when the content is
    /// shorter than the viewport.
    var distanceFromBottom: CGFloat { max(0, contentHeight - visibleBottom) }
}
