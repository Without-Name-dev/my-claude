import SwiftUI

/// Agent Tree View - Hierarchical visualization of agent executions
struct AgentTreeView: View {
    let agents: [AgentExecution]
    @State private var expandedAgents: Set<String> = []

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 0) {
                if agents.isEmpty {
                    ContentUnavailableView(
                        "没有 Agent 执行记录",
                        systemImage: "square.stack.3d.up.slash",
                        description: Text("当前会话中没有运行的 Agent")
                    )
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                } else {
                    ForEach(rootAgents) { agent in
                        AgentNodeView(
                            agent: agent,
                            children: childrenOf(agent.id),
                            level: 0,
                            expandedAgents: $expandedAgents
                        )
                    }
                }
            }
            .padding()
        }
    }

    var rootAgents: [AgentExecution] {
        agents.filter { $0.parentId == nil }
    }

    func childrenOf(_ parentId: String) -> [AgentExecution] {
        agents.filter { $0.parentId == parentId }
    }
}

struct AgentNodeView: View {
    let agent: AgentExecution
    let children: [AgentExecution]
    let level: Int
    @Binding var expandedAgents: Set<String>

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 8) {
                // Indentation
                if level > 0 {
                    ForEach(0..<level, id: \.self) { _ in
                        Rectangle()
                            .fill(Color.gray.opacity(0.3))
                            .frame(width: 2)
                    }
                    .frame(width: CGFloat(level * 20))
                }

                // Expand/collapse button
                if !children.isEmpty {
                    Button(action: { toggleExpanded() }) {
                        Image(systemName: isExpanded ? "chevron.down" : "chevron.right")
                            .font(.caption)
                            .foregroundColor(.secondary)
                    }
                    .buttonStyle(.plain)
                }

                // Status indicator
                StatusIndicator(status: agent.status)

                // Agent label
                VStack(alignment: .leading, spacing: 2) {
                    Text(agent.label)
                        .font(.body)
                        .fontWeight(level == 0 ? .semibold : .regular)

                    if let phase = agent.phase {
                        Text("Phase: \(phase)")
                            .font(.caption)
                            .foregroundColor(.secondary)
                    }
                }

                Spacer()

                // Token count
                if agent.tokenCount > 0 {
                    Text("\(agent.tokenCount) tokens")
                        .font(.caption)
                        .foregroundColor(.secondary)
                }

                // Duration
                if let started = agent.startedAt, let finished = agent.finishedAt {
                    let duration = finished.timeIntervalSince(started)
                    Text(formatDuration(duration))
                        .font(.caption)
                        .foregroundColor(.secondary)
                }
            }
            .padding(.vertical, 8)
            .padding(.horizontal, 12)
            .background(level == 0 ? Color(.systemGray6) : Color.clear)
            .cornerRadius(8)

            // Error message
            if let error = agent.errorMessage, !error.isEmpty {
                Text(error)
                    .font(.caption)
                    .foregroundColor(.red)
                    .padding(.leading, CGFloat((level + 1) * 20 + 12))
            }

            // Children
            if isExpanded && !children.isEmpty {
                ForEach(children) { child in
                    AgentNodeView(
                        agent: child,
                        children: childrenOf(child.id),
                        level: level + 1,
                        expandedAgents: $expandedAgents
                    )
                }
            }
        }
    }

    var isExpanded: Bool {
        expandedAgents.contains(agent.id)
    }

    func toggleExpanded() {
        if isExpanded {
            expandedAgents.remove(agent.id)
        } else {
            expandedAgents.insert(agent.id)
        }
    }

    func childrenOf(_ parentId: String) -> [AgentExecution] {
        children.filter { $0.parentId == parentId }
    }

    func formatDuration(_ seconds: TimeInterval) -> String {
        if seconds < 60 {
            return String(format: "%.1fs", seconds)
        } else {
            let minutes = Int(seconds / 60)
            let secs = Int(seconds.truncatingRemainder(dividingBy: 60))
            return "\(minutes)m \(secs)s"
        }
    }
}

struct StatusIndicator: View {
    let status: AgentStatus

    var body: some View {
        HStack(spacing: 4) {
            if status == .running {
                ProgressView()
                    .progressViewStyle(.circular)
                    .scaleEffect(0.6)
            } else {
                Image(systemName: icon)
                    .foregroundColor(color)
            }
        }
        .frame(width: 20, height: 20)
    }

    var icon: String {
        switch status {
        case .pending: return "clock"
        case .running: return "arrow.clockwise"
        case .completed: return "checkmark.circle.fill"
        case .error: return "xmark.circle.fill"
        case .cancelled: return "stop.circle.fill"
        }
    }

    var color: Color {
        switch status {
        case .pending: return .gray
        case .running: return .blue
        case .completed: return .green
        case .error: return .red
        case .cancelled: return .orange
        }
    }
}

/// Workflow Dashboard - Overview of workflow executions
struct WorkflowDashboardView: View {
    let workflows: [WorkflowRun]

    var body: some View {
        ScrollView {
            if workflows.isEmpty {
                ContentUnavailableView(
                    "没有工作流",
                    systemImage: "flowchart",
                    description: Text("当前会话中没有运行的工作流")
                )
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                LazyVStack(spacing: 16) {
                    ForEach(workflows) { workflow in
                        WorkflowCard(workflow: workflow)
                    }
                }
                .padding()
            }
        }
    }
}

struct WorkflowCard: View {
    let workflow: WorkflowRun

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            // Header
            HStack {
                VStack(alignment: .leading, spacing: 4) {
                    Text(workflow.name)
                        .font(.headline)

                    Text(workflow.startedAt, style: .relative)
                        .font(.caption)
                        .foregroundColor(.secondary)
                }

                Spacer()

                StatusBadge(status: workflow.status)
            }

            // Progress
            if let currentPhase = workflow.currentPhase {
                HStack {
                    Text("当前阶段:")
                        .font(.subheadline)
                        .foregroundColor(.secondary)

                    Text(currentPhase)
                        .font(.subheadline)
                        .fontWeight(.medium)
                }

                if workflow.phaseCount > 0 {
                    ProgressView(value: Double(workflow.agentCount), total: Double(workflow.phaseCount * 10))
                        .tint(.blue)
                }
            }

            // Statistics
            HStack(spacing: 24) {
                StatItem(icon: "square.stack.3d.up", label: "Agents", value: "\(workflow.agentCount)")
                StatItem(icon: "chart.bar", label: "Phases", value: "\(workflow.phaseCount)")
                StatItem(icon: "cpu", label: "Tokens", value: formatNumber(workflow.totalTokens))
            }

            // Error message
            if let error = workflow.errorMessage, !error.isEmpty {
                Text(error)
                    .font(.caption)
                    .foregroundColor(.red)
                    .padding(.top, 4)
            }
        }
        .padding()
        .background(Color(.systemGray6))
        .cornerRadius(12)
    }

    func formatNumber(_ num: Int) -> String {
        if num >= 1000000 {
            return String(format: "%.1fM", Double(num) / 1000000)
        } else if num >= 1000 {
            return String(format: "%.1fK", Double(num) / 1000)
        } else {
            return "\(num)"
        }
    }
}

struct StatItem: View {
    let icon: String
    let label: String
    let value: String

    var body: some View {
        VStack(spacing: 4) {
            HStack(spacing: 4) {
                Image(systemName: icon)
                    .font(.caption)
                Text(label)
                    .font(.caption)
            }
            .foregroundColor(.secondary)

            Text(value)
                .font(.subheadline)
                .fontWeight(.semibold)
        }
    }
}

struct WorkflowStatusBadge: View {
    let status: WorkflowStatus

    var body: some View {
        HStack(spacing: 4) {
            Circle()
                .fill(color)
                .frame(width: 8, height: 8)

            Text(status.displayName)
                .font(.caption)
                .fontWeight(.medium)
        }
        .padding(.horizontal, 8)
        .padding(.vertical, 4)
        .background(color.opacity(0.1))
        .cornerRadius(12)
    }

    var color: Color {
        switch status {
        case .running: return .blue
        case .completed: return .green
        case .error: return .red
        case .cancelled: return .gray
        }
    }
}
