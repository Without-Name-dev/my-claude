// Run with Swift 6.2:
// swiftc -o /tmp/scroll-check 'Agents Anywhere/Agents Anywhere/Models/Chat/TimelineViewport.swift' \
//   'Agents Anywhere/Agents Anywhere/Models/Chat/TimelineScrollState.swift' Tests/TimelineScrollStateChecks.swift
// /tmp/scroll-check
import Foundation

@main
struct TimelineScrollStateChecks {
    static func viewport(content: CGFloat = 1_000, height: CGFloat = 300,
                         bottom: CGFloat = 1_000, offset: CGFloat = 700) -> TimelineViewport {
        TimelineViewport(contentHeight: content, visibleHeight: height,
                         visibleBottom: bottom, offsetY: offset, topInset: 0)
    }

    static func openFollowing() -> TimelineScrollState {
        var state = TimelineScrollState()
        state.geometryChanged(viewport())
        state.open()
        guard let request = state.pendingBottomRequest, let command = state.begin(request) else {
            fatalError("opening must request the tail")
        }
        precondition(state.complete(command))
        precondition(state.mode == .following)
        return state
    }

    static func main() {
        var keyboard = openFollowing()
        keyboard.armKeyboardFollow()
        keyboard.geometryChanged(viewport(height: 0, bottom: 800))
        guard let first = keyboard.pendingBottomRequest, let command = keyboard.begin(first) else {
            fatalError("keyboard transition must pin the tail")
        }
        precondition(!first.animated)
        precondition(keyboard.complete(command))
        // Clamped visible height can remain zero while the keyboard moves.
        // The bottom edge still changes, so this must issue another pin.
        keyboard.geometryChanged(viewport(height: 0, bottom: 760))
        precondition(keyboard.requestBottom(animated: false))
        guard let next = keyboard.pendingBottomRequest else { fatalError("changed bottom edge lost") }
        precondition(next.bottomEdge != first.bottomEdge)
        guard let nextCommand = keyboard.begin(next) else { fatalError("second pin must start") }
        precondition(keyboard.complete(nextCommand))
        // The reverse transition (keyboard hiding) must also track the edge.
        keyboard.armKeyboardFollow()
        keyboard.geometryChanged(viewport(height: 0, bottom: 820))
        guard let hide = keyboard.pendingBottomRequest else { fatalError("keyboard hide lost") }
        precondition(!hide.animated && hide.bottomEdge != next.bottomEdge)

        var reading = openFollowing()
        let away = viewport(bottom: 600, offset: 300)
        reading.phaseChanged(.interacting, viewport: away)
        reading.phaseChanged(.idle, viewport: away)
        reading.settleUserScroll()
        precondition(reading.mode == .reading)
        reading.armKeyboardFollow()
        precondition(reading.mode == .reading && reading.pendingBottomRequest == nil)

        var atTail = openFollowing()
        atTail.browseHistory()
        atTail.armKeyboardFollow()
        precondition(atTail.returningToBottom)

        var approval = openFollowing()
        approval.setInteractionPresented(true)
        precondition(!approval.mayFollowNewContent)
        approval.geometryChanged(viewport(content: 1_200))
        approval.setInteractionPresented(false)
        precondition(approval.returningToBottom)

        var approvalReading = openFollowing()
        approvalReading.setInteractionPresented(true)
        approvalReading.phaseChanged(.interacting, viewport: away)
        approvalReading.phaseChanged(.idle, viewport: away)
        approvalReading.settleUserScroll()
        approvalReading.setInteractionPresented(false)
        precondition(approvalReading.mode == .reading)
        print("Timeline scroll state checks passed")
    }
}
