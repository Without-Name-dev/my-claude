import UIKit

/// Copy to the pasteboard with the app-wide success feedback. Some copy
/// actions had the haptic and the checkmark morph, others were completely
/// silent; one helper makes the gesture feel the same everywhere.
enum CopyAction {
    @MainActor static func copy(_ value: String) {
        UIPasteboard.general.string = value
        UINotificationFeedbackGenerator().notificationOccurred(.success)
    }
}
