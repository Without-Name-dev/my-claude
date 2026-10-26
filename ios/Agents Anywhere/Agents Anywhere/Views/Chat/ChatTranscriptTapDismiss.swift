import SwiftUI
import UIKit

/// Dismisses the keyboard when the person taps the transcript.
///
/// A SwiftUI tap gesture attached to the scroll view never fires for taps that
/// land inside message text: the text selection overlay built by Textual
/// recognises the touch first, and the scroll view's own pan recogniser wins
/// over a simultaneous tap. The recogniser therefore lives on the underlying
/// UIScrollView and never cancels touches, so buttons, links, attachments and
/// text selection keep behaving exactly as before while the keyboard still
/// closes.
struct ChatTranscriptTapDismiss: UIViewRepresentable {
    var onTap: () -> Void

    func makeCoordinator() -> Coordinator { Coordinator(onTap: onTap) }

    func makeUIView(context: Context) -> UIView {
        // Only a anchor in the hierarchy: the recogniser goes on the scroll
        // view found above it, and this view never receives touches.
        let view = UIView()
        view.backgroundColor = .clear
        view.isUserInteractionEnabled = false
        context.coordinator.host = view
        context.coordinator.attach()
        return view
    }

    func updateUIView(_ uiView: UIView, context: Context) {
        context.coordinator.onTap = onTap
        context.coordinator.host = uiView
        context.coordinator.attach()
    }

    final class Coordinator: NSObject, UIGestureRecognizerDelegate {
        weak var host: UIView?
        var onTap: () -> Void
        private weak var scrollView: UIScrollView?
        private var recognizer: UITapGestureRecognizer?

        init(onTap: @escaping () -> Void) { self.onTap = onTap }

        /// Installs the recogniser once the host is in the hierarchy. The
        /// scroll view is an ancestor, so it only exists after layout.
        func attach() {
            guard recognizer == nil, let host else { return }
            DispatchQueue.main.async { [weak self, weak host] in
                guard let self, let host, self.recognizer == nil,
                      let scrollView = Self.enclosingScrollView(from: host) else { return }
                // The session title/status bar is not a navigation command.
                // UIKit's default scroll-to-top action would discard the
                // reader's position when that area is tapped.
                scrollView.scrollsToTop = false
                let recognizer = UITapGestureRecognizer(target: self, action: #selector(self.handleTap))
                // The content keeps every touch it would otherwise have had.
                recognizer.cancelsTouchesInView = false
                recognizer.delegate = self
                scrollView.addGestureRecognizer(recognizer)
                self.scrollView = scrollView
                self.recognizer = recognizer
            }
        }

        @objc private func handleTap() {
            onTap()
        }

        func gestureRecognizer(
            _ gestureRecognizer: UIGestureRecognizer,
            shouldRecognizeSimultaneouslyWith other: UIGestureRecognizer
        ) -> Bool {
            true
        }

        func gestureRecognizer(_ gestureRecognizer: UIGestureRecognizer, shouldReceive touch: UITouch) -> Bool {
            // Taps that begin outside the transcript — the composer, the
            // navigation bar, a sheet — are not a request to stop writing.
            guard let scrollView else { return false }
            return touch.view === scrollView || touch.view?.isDescendant(of: scrollView) == true
        }

        private static func enclosingScrollView(from view: UIView) -> UIScrollView? {
            var current: UIView? = view.superview
            while let candidate = current {
                if let scrollView = candidate as? UIScrollView { return scrollView }
                current = candidate.superview
            }
            return nil
        }
    }
}
