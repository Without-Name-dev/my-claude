import SwiftUI

/// One lazy row in the transcript. The header reuses `TimelineMarkerRow` so the
/// shimmer, spacing and monospaced caption match tool and reasoning rows.
struct SubagentActivityRow: View {
    let activity: SubagentActivityPresentation
    @State private var expanded = false
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Group {
            if activity.isRunning {
                // Durations need a one-second clock. Only the visible row runs
                // it; the timeline model is not republished for every tick.
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    content(now: context.date)
                }
            } else {
                content(now: Date())
            }
        }
        .onChange(of: activity.epoch) { _, _ in expanded = false }
        .onChange(of: activity.isRunning) { _, running in
            // The server reports an empty set when all subagents stop. A
            // finished block collapses to its one-line summary.
            if !running { expanded = false }
        }
        .accessibilityIdentifier("chat.subagents")
    }

    private func content(now: Date) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Button {
                withAnimation(reduceMotion ? nil : .easeInOut(duration: 0.18)) {
                    expanded.toggle()
                }
            } label: {
                TimelineMarkerRow(title: activity.headerTitle(now: now), symbol: "person.2",
                    status: activity.markerStatus, expanded: expanded)
            }
            .buttonStyle(.plain)
            .accessibilityHint(expanded ? String(localized: "轻点收起") : String(localized: "轻点展开"))

            if expanded {
                VStack(alignment: .leading, spacing: 2) {
                    ForEach(activity.agents) { agent in
                        VStack(alignment: .leading, spacing: 2) {
                            TimelineMarkerRow(title: activity.rowTitle(agent, now: now),
                                symbol: "person.crop.circle", status: agent.markerStatus)
                            if agent.isRunning, let toolLine = agent.toolLine {
                                // What the terminal draws under the agent: the
                                // tool it is in, and the one-line argument.
                                Text(toolLine)
                                    .font(.system(.caption, design: .monospaced))
                                    .foregroundStyle(.secondary)
                                    .lineLimit(1)
                                    .truncationMode(.middle)
                                    .padding(.leading, 26)
                            }
                            if let text = agent.latestText {
                                Text(text)
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                                    .lineLimit(6)
                                    .multilineTextAlignment(.leading)
                                    .padding(.leading, 26)
                            }
                        }
                        .padding(.leading, 24)
                    }
                }
                .transition(.identity)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}
