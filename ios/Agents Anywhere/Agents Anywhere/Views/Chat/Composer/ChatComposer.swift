import SwiftUI

struct ChatComposer: View {
    @Bindable var draft: ComposerDraft
    let editor: ComposerEditorController
    let isStreaming: Bool
    var canSend = true
    var canStop = true
    var isBusy = false
    /// Photo/file import is running. It blocks sending but must not block stop.
    var isImporting = false
    var placeholder = String(localized: "询问 Agents")
    let maximumEditorHeight: CGFloat
    let controls: ChatControlMetrics
    let onSend: () -> Void
    let onStop: () -> Void
    let onOptions: () -> Void
    /// Shown beside send when the session's runtime offers slash commands.
    var showsCommands = false
    var commandsActive = false
    var onCommands: () -> Void = {}
    /// Reports draft mutations from this subtree only. Persisting the draft
    /// must not make the page root observe the editor's text.
    var onDraftChange: () -> Void = {}
    @Environment(\.colorScheme) private var colorScheme
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Namespace private var glass
    /// Haptics fire from these counters; sending is the most repeated action
    /// in the app and used to be completely silent.
    @State private var sendFeedback = 0
    @State private var stopFeedback = 0
    /// A tap on Send while the IME candidate bar is open is not a send: the
    /// text is still marked and may change under the person. The button stays
    /// enabled so the tap can say why nothing happened instead of looking dead.
    @State private var composingHint = false
    @State private var composingHintRevision = 0

    private var hasSendableContent: Bool { draft.hasSendableContent }
    private var showsStopAction: Bool { !hasSendableContent && isStreaming }
    private var primaryActionEnabled: Bool {
        if hasSendableContent { return canSend && !isBusy }
        if isStreaming { return canStop && !isBusy }
        return false
    }

    var body: some View {
        GlassEffectContainer(spacing: 12) {
            VStack(spacing: 0) {
                if composingHint {
                    Text(String(localized: "请先选定输入法候选字再发送"))
                        .font(.caption).foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity)
                        .padding(.bottom, 2)
                        .transition(.opacity)
                }
                if !draft.attachments.isEmpty || isImporting { attachmentTray }
                ComposerLayout(expanded: draft.isExpanded, maximumEditorHeight: maximumEditorHeight, controls: controls) {
                    Button(action: onOptions) {
                        AppSymbol("plus", size: 24)
                            .frame(width: controls.touchTarget, height: controls.touchTarget)
                            .contentShape(Circle())
                    }
                    .buttonStyle(.plain)
                    .accessibilityLabel(String(localized: "附件与对话选项"))
                    .accessibilityIdentifier("chat.composer.options")

                    ZStack(alignment: .topLeading) {
                        if draft.text.isEmpty {
                            Text(placeholder)
                                .font(.body)
                                .lineLimit(1)
                                .foregroundStyle(.secondary)
                                .allowsHitTesting(false)
                        }
                        NativeComposerEditor(draft: draft, controller: editor, maximumHeight: maximumEditorHeight, onCommandSend: onSend)
                    }

                    Button {
                        if hasSendableContent {
                            if draft.isComposing { showComposingHint(); return }
                            sendFeedback &+= 1; onSend()
                        } else if showsStopAction { stopFeedback &+= 1; onStop() }
                    } label: {
                        ZStack {
                            AppSymbol("arrow.up", size: 18)
                                .opacity(showsStopAction ? 0 : 1)
                                .scaleEffect(showsStopAction ? 0.7 : 1)
                            AppSymbol("stop.fill", size: 13)
                                .opacity(showsStopAction ? 1 : 0)
                                .scaleEffect(showsStopAction ? 1 : 0.7)
                        }
                            .foregroundStyle(showsStopAction ? Color.red : AppTheme.primaryControlForeground(colorScheme))
                            .frame(width: controls.sendDiameter, height: controls.sendDiameter)
                            .background(
                                showsStopAction
                                    ? Color.red.opacity(primaryActionEnabled ? 0.12 : 0.06)
                                    : AppTheme.primaryControlBackground(colorScheme).opacity(primaryActionEnabled ? 1 : 0.42),
                                in: Circle()
                            )
                            .frame(width: controls.touchTarget, height: controls.touchTarget)
                            .contentShape(Circle())
                            .animation(reduceMotion ? nil : .snappy(duration: 0.2), value: showsStopAction)
                    }
                    .buttonStyle(.plain)
                    .disabled(!primaryActionEnabled)
                    .accessibilityLabel(String(localized: showsStopAction ? "停止生成" : "发送消息"))
                    .accessibilityHint(draft.isComposing ? String(localized: "请先确认输入法候选文字") : "")
                    .accessibilityIdentifier(showsStopAction ? "chat.composer.stop" : "chat.composer.send")

                    // A draft used to hide the only stop control: typing
                    // anything while the agent was working removed the one way
                    // to cancel it. Keep stop beside send whenever the turn is
                    // still running and the primary button has become send.
                    //
                    // Declared after the primary button on purpose:
                    // ComposerLayout places subviews[2] at the right edge, so
                    // declaring stop first put Stop under the thumb that
                    // expected Send and interrupted the agent instead.
                    if isStreaming && hasSendableContent {
                        Button { stopFeedback &+= 1; onStop() } label: {
                            AppSymbol("stop.fill", size: 13)
                                .foregroundStyle(Color.red)
                                .frame(width: controls.sendDiameter, height: controls.sendDiameter)
                                .background(Color.red.opacity(canStop && !isBusy ? 0.12 : 0.06), in: Circle())
                                .frame(width: controls.touchTarget, height: controls.touchTarget)
                                .contentShape(Circle())
                        }
                        .buttonStyle(.plain)
                        .disabled(!canStop || isBusy)
                        .accessibilityLabel(String(localized: "停止生成"))
                        .accessibilityIdentifier("chat.composer.stop.secondary")
                    }

                    if showsCommands {
                        Button(action: onCommands) {
                            AppSymbol("command", size: 18)
                                .foregroundStyle(commandsActive ? AnyShapeStyle(.tint) : AnyShapeStyle(.secondary))
                                .frame(width: controls.touchTarget, height: controls.touchTarget)
                                .contentShape(Circle())
                        }
                        .buttonStyle(.plain)
                        .accessibilityLabel(String(localized: "指令"))
                        .accessibilityAddTraits(commandsActive ? .isSelected : [])
                        .accessibilityIdentifier("chat.composer.commands")
                    }
                }
            }
            .glassEffect(.regular.interactive(), in: .rect(cornerRadius: draft.isExpanded ? controls.expandedCornerRadius : controls.collapsedCornerRadius))
            .glassEffectID("composer", in: glass)
        }
        .padding(.horizontal, draft.isExpanded ? ChatControlMetrics.expandedHorizontalInset : ChatControlMetrics.collapsedHorizontalInset)
        .padding(.top, 8)
        .padding(.bottom, 10)
        .sensoryFeedback(.impact(weight: .medium), trigger: sendFeedback)
        .sensoryFeedback(.impact(flexibility: .rigid), trigger: stopFeedback)
        .task(id: composingHintRevision) {
            guard composingHint else { return }
            do { try await Task.sleep(for: .seconds(1.6)) } catch { return }
            guard !Task.isCancelled else { return }
            withAnimation(reduceMotion ? nil : .easeOut(duration: 0.15)) { composingHint = false }
        }
        .animation(reduceMotion ? nil : .smooth(duration: 0.24), value: draft.isExpanded)
        // A second or third attachment changes the tray's height even when it is
        // already expanded; the fold animation above does not cover that.
        .animation(reduceMotion ? nil : .smooth(duration: 0.2), value: draft.attachments.map(\.id))
        // The composer is the only subtree that already re-evaluates on every
        // keystroke, including each Chinese IME marked-text update. Observing
        // the draft from the page root re-evaluated the whole conversation.
        .onChange(of: draft.text) { _, _ in onDraftChange() }
        .onChange(of: draft.attachments) { _, _ in onDraftChange() }
    }

    private func showComposingHint() {
        withAnimation(reduceMotion ? nil : .easeOut(duration: 0.15)) { composingHint = true }
        composingHintRevision &+= 1
    }

    private var attachmentTray: some View {
        ScrollView(.horizontal) {
            HStack(spacing: 8) {
                ForEach(draft.attachments) { attachment in
                    ChatComposerAttachment(attachment: attachment) {
                        draft.attachments.removeAll { $0.id == attachment.id }
                    }
                    .transition(.scale(scale: 0.9).combined(with: .opacity))
                }
                if isImporting {
                    HStack(spacing: 6) {
                        ProgressView().controlSize(.small)
                        Text(String(localized: "正在导入…")).font(.caption).foregroundStyle(.secondary)
                    }
                    .padding(.horizontal, 10)
                    .frame(height: 56)
                    .transition(.opacity)
                }
            }
        }
        .scrollIndicators(.hidden)
        .padding(.horizontal, 12)
        .padding(.top, 12)
    }
}
