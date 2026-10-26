import Foundation
import Observation

/// A tiny on-device recorder for the transcript's scroll behaviour.
///
/// The chat page has jumped on the phone since it was written, and every fix
/// attempted from code reading alone has been a guess. This records the raw
/// numbers of each scroll event — phase, offset, content and viewport height,
/// and what asked the page to move — and shows the last few lines over the
/// transcript, so one screenshot at the moment of a jump says which event did
/// it. Off by default; the trace only runs while the switch is on.
@MainActor
@Observable
final class ChatScrollLog {
    static let shared = ChatScrollLog()

    private static let enabledKey = "chat.scrollLog.enabled"
    /// Enough to see a gesture and what followed it.
    private static let limit = 40

    var isEnabled: Bool {
        didSet { UserDefaults.standard.set(isEnabled, forKey: Self.enabledKey) }
    }
    private(set) var lines: [String] = []

    private init() {
        isEnabled = UserDefaults.standard.bool(forKey: Self.enabledKey)
    }

    func record(_ message: String) {
        guard isEnabled else { return }
        lines.append(message)
        if lines.count > Self.limit {
            lines.removeFirst(lines.count - Self.limit)
        }
    }

    func clear() {
        lines.removeAll()
    }

    /// The few lines the overlay shows; the newest last.
    var visibleLines: [String] {
        Array(lines.suffix(3))
    }
}
