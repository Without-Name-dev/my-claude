import SwiftUI
import UIKit

/// Keep this controller and the underlying UITextView alive while the glass
/// container changes layout. Never derive submission from SwiftUI .onSubmit.
@MainActor @Observable
final class ComposerEditorController {
    @ObservationIgnored weak var textView: ComposerTextView?
    @ObservationIgnored weak var draft: ComposerDraft?
    @ObservationIgnored var onFinishEditing: (() -> Void)?

    func finishEditing() {
        onFinishEditing?()
        guard let textView else { return }
        textView.resignFirstResponder()
        synchronize()
    }

    /// Check native marked text before submitting. Never end editing here: the
    /// old implementation resigned first responder to force the IME to commit,
    /// which dismissed the keyboard after every send. A marked composition
    /// keeps ownership of the field, so the caller ignores this attempt and the
    /// editor stays focused for the next message. UIKit exposes no public
    /// dictation-active state; keeping first responder is what prevents the
    /// keyboard from collapsing when dictation is in progress.
    func committedTextForSend() async -> String? {
        guard let textView else { return draft?.isComposing == false ? draft?.text : nil }
        guard textView.markedTextRange == nil else { synchronize(); return nil }
        synchronize()
        return textView.text ?? ""
    }

    func synchronize() {
        guard let textView, let draft, draft.isValid else { return }
        if draft.text != textView.text { draft.text = textView.text ?? "" }
        draft.isComposing = textView.markedTextRange != nil
        draft.isFocused = textView.isFirstResponder
    }
}

final class ComposerTextView: UITextView {
    var commandSend: (() -> Void)?

    override var keyCommands: [UIKeyCommand]? {
        let send = UIKeyCommand(title: String(localized: "发送"), action: #selector(sendWithCommandReturn), input: "\r", modifierFlags: .command)
        send.wantsPriorityOverSystemBehavior = true
        return (super.keyCommands ?? []) + [send]
    }

    @objc private func sendWithCommandReturn() {
        // Leave provisional text and the candidate bar intact. Return/Space or
        // a candidate tap remains owned by the IME; this shortcut never guesses.
        guard markedTextRange == nil else { return }
        commandSend?()
    }
}

struct NativeComposerEditor: UIViewRepresentable {
    @Bindable var draft: ComposerDraft
    let controller: ComposerEditorController
    let maximumHeight: CGFloat
    let onCommandSend: () -> Void
    var onTextChange: ((String, Bool) -> Void)? = nil

    func makeUIView(context: Context) -> ComposerTextView {
        let view = ComposerTextView()
        view.delegate = context.coordinator
        view.backgroundColor = .clear
        view.font = .preferredFont(forTextStyle: .body)
        view.adjustsFontForContentSizeCategory = true
        view.textColor = .label
        view.tintColor = .label
        view.returnKeyType = .default
        view.enablesReturnKeyAutomatically = false
        view.textContainerInset = .zero
        view.textContainer.lineFragmentPadding = 0
        view.contentInset = .zero
        view.isScrollEnabled = false
        view.showsVerticalScrollIndicator = true
        view.keyboardDismissMode = .none
        view.setContentCompressionResistancePriority(.defaultLow, for: .horizontal)
        view.accessibilityLabel = String(localized: "消息")
        view.accessibilityHint = String(localized: "回车换行，Command 加回车发送。")
        view.accessibilityIdentifier = "chat.composer.editor"
        controller.textView = view; controller.draft = draft
        controller.onFinishEditing = { [weak coordinator = context.coordinator] in
            coordinator?.cancelPendingRefocus()
        }
        return view
    }

    func updateUIView(_ view: ComposerTextView, context: Context) {
        context.coordinator.parent = self
        controller.textView = view; controller.draft = draft
        view.commandSend = onCommandSend
        // Do not replace native pre-edit text or disrupt its selection/candidate bar.
        if view.markedTextRange == nil, view.text != draft.text {
            let previous = view.text ?? ""
            let selection = view.selectedRange
            view.text = draft.text
            let end = (draft.text as NSString).length
            // The saved caret belonged to the old string. When the new text
            // merely extends it (a keystroke echoed back) the offset is still
            // valid; a whole-string replacement — command completion, a
            // restored draft — must put the caret after the new text, or the
            // next character lands inside the command name.
            if !previous.isEmpty && draft.text.hasPrefix(previous) {
                view.selectedRange = NSRange(location: min(selection.location, end), length: 0)
            } else {
                view.selectedRange = NSRange(location: end, length: 0)
            }
        }
        // Focus only on a rising edge of the desired state. Re-asserting
        // becomeFirstResponder on every update resurrected the keyboard right
        // after an interactive dismissal (scroll-dismiss or drag-to-close),
        // which is why the composer would not collapse back to the pill.
        let coordinator = context.coordinator
        if draft.isFocused != coordinator.desiredFocus {
            coordinator.desiredFocus = draft.isFocused
            if draft.isFocused {
                view.becomeFirstResponder()
            } else if view.isFirstResponder {
                view.resignFirstResponder()
            }
        }
    }

    func sizeThatFits(_ proposal: ProposedViewSize, uiView: ComposerTextView, context: Context) -> CGSize? {
        guard let width = proposal.width, width > 0 else { return nil }
        let measured = uiView.sizeThatFits(CGSize(width: width, height: .greatestFiniteMagnitude)).height
        let minimum = ceil(uiView.font?.lineHeight ?? 24)
        let height = min(maximumHeight, max(minimum, ceil(measured)))
        // Publish the measured height so the composer can animate line growth
        // (no animation value covered it; the inset stepped in one frame).
        if draft.measuredEditorHeight != height {
            draft.measuredEditorHeight = height
        }
        let shouldScroll = measured > maximumHeight + 1
        if uiView.isScrollEnabled != shouldScroll { uiView.isScrollEnabled = shouldScroll }
        return CGSize(width: width, height: height)
    }

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    final class Coordinator: NSObject, UITextViewDelegate {
        var parent: NativeComposerEditor
        /// The desired focus state this view last acted on. Nil until the
        /// first update, so an initial draft.isFocused == true still focuses.
        var desiredFocus: Bool?
        /// Whether the editor held focus when the app deactivated. A
        /// screenshot or a notification-center pull resigns the first
        /// responder during the snapshot; without remembering it, the
        /// keyboard never came back afterwards.
        private var refocusOnActivation = false
        private var observers: [NSObjectProtocol] = []
        init(_ parent: NativeComposerEditor) {
            self.parent = parent
            super.init()
            observers.append(NotificationCenter.default.addObserver(
                forName: UIApplication.willResignActiveNotification, object: nil, queue: .main
            ) { [weak self] _ in
                guard let self else { return }
                self.refocusOnActivation = self.parent.controller.textView?.isFirstResponder == true
            })
            observers.append(NotificationCenter.default.addObserver(
                forName: UIApplication.didBecomeActiveNotification, object: nil, queue: .main
            ) { [weak self] _ in
                guard let self, self.refocusOnActivation else { return }
                self.refocusOnActivation = false
                // The user may have tapped elsewhere meanwhile; only the
                // editor that still owns the draft's focus state returns.
                if self.parent.draft.isValid,
                   let textView = self.parent.controller.textView, textView.window != nil,
                   self.parent.draft.isFocused || self.parent.draft.isExpanded {
                    textView.becomeFirstResponder()
                }
            })
        }
        deinit { observers.forEach(NotificationCenter.default.removeObserver) }
        func cancelPendingRefocus() { refocusOnActivation = false }
        func textViewDidChange(_ textView: UITextView) {
            synchronize()
            textView.invalidateIntrinsicContentSize()
        }
        func textViewDidChangeSelection(_ textView: UITextView) { synchronize() }
        func textViewDidBeginEditing(_ textView: UITextView) { synchronize() }
        func textViewDidEndEditing(_ textView: UITextView) { synchronize() }
        private func synchronize() {
            parent.controller.synchronize()
            parent.onTextChange?(parent.draft.text, parent.draft.isComposing)
        }
        func textView(_ textView: UITextView, shouldChangeTextIn range: NSRange, replacementText text: String) -> Bool {
            // UIKit owns IME candidate confirmation. A plain Return is always
            // accepted as text input, never interpreted as a send gesture.
            true
        }
    }
}
