import SwiftUI

/// Message Queue Bar - Displays queue status above composer
struct MessageQueueBar: View {
    @ObservedObject var repository: MessageQueueRepository
    @State private var showPanel = false

    var body: some View {
        if !repository.messages.isEmpty {
            Button(action: { showPanel = true }) {
                HStack(spacing: 12) {
                    if let running = repository.messages.first(where: { $0.status == .running }) {
                        ProgressView()
                            .progressViewStyle(.circular)
                            .scaleEffect(0.8)

                        Text("执行中: \(running.content.prefix(20))...")
                            .lineLimit(1)
                            .font(.subheadline)
                    } else {
                        let queued = repository.messages.filter { $0.status == .queued }
                        if !queued.isEmpty {
                            Image(systemName: "clock")
                            Text("排队中 (\(queued.count))")
                                .font(.subheadline)
                        }
                    }

                    Spacer()

                    Image(systemName: "chevron.up")
                        .font(.caption)
                }
                .padding(.horizontal, 16)
                .padding(.vertical, 12)
                .background(Color(.systemGray6))
                .cornerRadius(8)
            }
            .buttonStyle(.plain)
            .padding(.horizontal)
            .sheet(isPresented: $showPanel) {
                MessageQueuePanel(repository: repository)
            }
        }
    }
}

/// Message Queue Panel - Full queue management
struct MessageQueuePanel: View {
    @ObservedObject var repository: MessageQueueRepository
    @Environment(\.dismiss) var dismiss

    var body: some View {
        NavigationView {
            List {
                ForEach(repository.messages) { message in
                    MessageQueueRow(message: message, repository: repository)
                }
            }
            .navigationTitle("消息队列")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("关闭") { dismiss() }
                }

                ToolbarItem(placement: .navigationBarTrailing) {
                    Button(action: { Task { try? await repository.fetchMessages() } }) {
                        Image(systemName: "arrow.clockwise")
                    }
                }
            }
            .overlay {
                if repository.messages.isEmpty {
                    ContentUnavailableView(
                        "队列为空",
                        systemImage: "tray",
                        description: Text("没有排队的消息")
                    )
                }
            }
        }
    }
}

/// Message Queue Row
struct MessageQueueRow: View {
    let message: QueuedMessage
    @ObservedObject var repository: MessageQueueRepository

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                StatusBadge(status: message.status)

                Text(message.content)
                    .lineLimit(2)
                    .font(.body)

                Spacer()

                if message.status == .running {
                    ProgressView()
                        .progressViewStyle(.circular)
                        .scaleEffect(0.8)
                }
            }

            HStack {
                Text(message.createdAt, style: .relative)
                    .font(.caption)
                    .foregroundColor(.secondary)

                Spacer()

                if message.status == .queued {
                    Button("取消") {
                        Task {
                            try? await repository.cancel(messageID: message.id)
                        }
                    }
                    .font(.caption)
                    .buttonStyle(.bordered)
                }
            }

            if let error = message.errorMessage {
                Text(error)
                    .font(.caption)
                    .foregroundColor(.red)
                    .padding(.top, 4)
            }
        }
        .padding(.vertical, 4)
    }
}

/// Status Badge
struct StatusBadge: View {
    let status: QueueStatus

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
        case .queued: return .yellow
        case .running: return .blue
        case .completed: return .green
        case .error: return .red
        case .cancelled: return .gray
        }
    }
}
