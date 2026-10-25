import SwiftUI

/// Measures and places Textual with the same column width in one layout pass.
/// No geometry callback writes into View state or schedules a second layout.
struct MarkdownBlockLayout: Layout {
    let dynamicType: DynamicTypeSize
    let displayScale: CGFloat
    let direction: LayoutDirection
    /// Bumped by an authoritative text replacement. Including it in the cache
    /// resets the height reservation without recreating the enclosing view, so
    /// Textual's selection model (and any in-progress selection) survives.
    let generation: Int

    struct Cache {
        var generation: Int
        var dynamicType: DynamicTypeSize
        var displayScale: CGFloat
        var direction: LayoutDirection
        var sizing = MarkdownBlockSizing()
        var measurements = MarkdownMeasurementCache()
    }

    func makeCache(subviews: Subviews) -> Cache {
        Cache(generation: generation, dynamicType: dynamicType, displayScale: displayScale, direction: direction)
    }

    func updateCache(_ cache: inout Cache, subviews: Subviews) {
        if cache.generation != generation || cache.dynamicType != dynamicType
            || cache.displayScale != displayScale || cache.direction != direction {
            cache = makeCache(subviews: subviews)
        }
        // A subview change (including asynchronous attachments/highlighting)
        // invalidates actual measurements, while scroll/translation alone does not.
        cache.measurements.invalidate()
        // Appends and Textual's asynchronous highlighter/attachment updates
        // retain the last real height. An authoritative replacement bumps the
        // generation, which resets this cache without recreating the view.
    }

    func sizeThatFits(proposal: ProposedViewSize, subviews: Subviews, cache: inout Cache) -> CGSize {
        guard let content = subviews.first else { return .zero }
        let width = proposal.width.flatMap { $0.isFinite ? max(0, $0) : nil }
        let natural = cache.measurements.size(proposedWidth: width) {
            content.sizeThatFits(.init(width: width, height: nil))
        }
        return cache.sizing.size(proposedWidth: width, naturalSize: natural, displayScale: displayScale)
    }

    func placeSubviews(in bounds: CGRect, proposal: ProposedViewSize, subviews: Subviews, cache: inout Cache) {
        subviews.first?.place(at: bounds.origin, anchor: .topLeading,
            proposal: .init(width: bounds.width, height: nil))
    }
}
