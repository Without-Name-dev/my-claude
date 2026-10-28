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
    }

    /// Builds from the scroll view's visible region.
    ///
    /// Subtracting insets from the container height breaks with the keyboard
    /// up: SwiftUI both shrinks the container and adds the keyboard to the
    /// content insets, so the difference collapsed to zero (`vh=0` on device).
    /// `isMeasured` then became false, which disabled every follow and return
    /// path exactly when the person was typing and needed them.
    init(contentHeight: CGFloat, visibleHeight: CGFloat, visibleBottom: CGFloat,
         offsetY: CGFloat, topInset: CGFloat) {
        self.contentHeight = max(0, contentHeight)
        self.visibleHeight = max(0, visibleHeight)
        self.visibleBottom = visibleBottom
        self.offsetY = offsetY
        self.topInset = topInset
    }
    var isMeasured: Bool { visibleHeight > 0 }
    /// How far the bottom edge of the viewport sits above the end of the
    /// content. Zero when the reader is at the tail or when the content is
    /// shorter than the viewport.
    var distanceFromBottom: CGFloat { max(0, contentHeight - visibleBottom) }
}
