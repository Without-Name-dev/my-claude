import SwiftUI

struct TerminalListView: View {
    let connectorId: String
    let connectorName: String

    @State private var terminals: [V2Terminal] = []
    @State private var isLoading = false
    @State private var error: Error?
    @Environment(\.services) private var services

    var body: some View {
        Group {
            if isLoading && terminals.isEmpty {
                ProgressView("加载中...")
            } else if let error = error, terminals.isEmpty {
                ContentUnavailableView {
                    Label("加载失败", systemImage: "exclamationmark.triangle")
                } description: {
                    Text(error.localizedDescription)
                } actions: {
                    Button("重试") {
                        Task { await loadTerminals() }
                    }
                }
            } else if terminals.isEmpty {
                ContentUnavailableView {
                    Label("无终端", systemImage: "terminal")
                } description: {
                    Text("此设备上没有运行的终端")
                }
            } else {
                List {
                    ForEach(terminals) { terminal in
                        NavigationLink {
                            TerminalDetailView(terminal: terminal)
                        } label: {
                            TerminalRowView(terminal: terminal)
                        }
                    }
                }
                .refreshable {
                    await loadTerminals()
                }
            }
        }
        .navigationTitle("终端 - \(connectorName)")
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Button {
                    Task { await loadTerminals() }
                } label: {
                    Image(systemName: "arrow.clockwise")
                }
                .disabled(isLoading)
            }
        }
        .task {
            await loadTerminals()
        }
    }

    private func loadTerminals() async {
        isLoading = true
        error = nil

        do {
            // Note: 这里需要从服务端获取终端列表
            // 暂时使用空数组，等待 API 端点实现
            terminals = []
            // terminals = try await services.terminalService.listTerminals(connectorId: connectorId)
        } catch {
            self.error = error
        }

        isLoading = false
    }
}

struct TerminalRowView: View {
    let terminal: V2Terminal

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack {
                Text(terminal.label.isEmpty ? "终端" : terminal.label)
                    .font(.headline)

                Spacer()

                StatusBadge(status: terminal.status)
            }

            HStack(spacing: 16) {
                Label {
                    Text(terminal.shell)
                } icon: {
                    Image(systemName: "terminal")
                        .foregroundStyle(.secondary)
                }
                .font(.caption)
                .foregroundStyle(.secondary)

                if terminal.pid > 0 {
                    Label {
                        Text("PID: \(terminal.pid)")
                    } icon: {
                        Image(systemName: "number")
                            .foregroundStyle(.secondary)
                    }
                    .font(.caption)
                    .foregroundStyle(.secondary)
                }
            }

            if !terminal.workingDirectory.isEmpty {
                Label {
                    Text(terminal.workingDirectory)
                        .lineLimit(1)
                        .truncationMode(.middle)
                } icon: {
                    Image(systemName: "folder")
                        .foregroundStyle(.secondary)
                }
                .font(.caption)
                .foregroundStyle(.secondary)
            }

            Text(formatTimestamp(terminal.updatedAt))
                .font(.caption2)
                .foregroundStyle(.tertiary)
        }
        .padding(.vertical, 4)
    }

    private func formatTimestamp(_ timestamp: Int64) -> String {
        let date = Date(timeIntervalSince1970: TimeInterval(timestamp) / 1000)
        let formatter = RelativeDateTimeFormatter()
        formatter.unitsStyle = .abbreviated
        return formatter.localizedString(for: date, relativeTo: Date())
    }
}

struct StatusBadge: View {
    let status: String

    var body: some View {
        Text(statusText)
            .font(.caption2)
            .fontWeight(.medium)
            .padding(.horizontal, 8)
            .padding(.vertical, 4)
            .background(backgroundColor)
            .foregroundStyle(foregroundColor)
            .clipShape(Capsule())
    }

    private var statusText: String {
        switch status {
        case "starting": return "启动中"
        case "running": return "运行中"
        case "exited": return "已退出"
        default: return status
        }
    }

    private var backgroundColor: Color {
        switch status {
        case "starting": return .yellow.opacity(0.2)
        case "running": return .green.opacity(0.2)
        case "exited": return .gray.opacity(0.2)
        default: return .gray.opacity(0.2)
        }
    }

    private var foregroundColor: Color {
        switch status {
        case "starting": return .yellow
        case "running": return .green
        case "exited": return .gray
        default: return .gray
        }
    }
}
