import SwiftUI

/// Command Palette - Cmd+K command execution interface
struct CommandPaletteView: View {
    @ObservedObject var configRepository: SessionConfigRepository
    @Environment(\.dismiss) var dismiss
    @State private var searchText = ""
    @State private var selectedCommand: Command?
    @State private var isExecuting = false
    @State private var result: String?
    @State private var error: Error?

    var body: some View {
        NavigationView {
            VStack(spacing: 0) {
                SearchBar(text: $searchText)
                    .padding()

                List(filteredCommands) { command in
                    CommandRow(command: command)
                        .onTapGesture {
                            executeCommand(command)
                        }
                }
            }
            .navigationTitle("命令")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .navigationBarLeading) {
                    Button("取消") { dismiss() }
                }
            }
            .overlay {
                if isExecuting {
                    ProgressView("执行中...")
                        .padding()
                        .background(.ultraThinMaterial)
                        .cornerRadius(12)
                }
            }
            .alert("命令结果", isPresented: .constant(result != nil)) {
                Button("确定") { result = nil }
            } message: {
                if let result = result {
                    Text(result)
                }
            }
            .alert("错误", isPresented: .constant(error != nil)) {
                Button("确定") { error = nil }
            } message: {
                if let error = error {
                    Text(error.localizedDescription)
                }
            }
        }
    }

    var filteredCommands: [Command] {
        if searchText.isEmpty {
            return commands
        }
        return commands.filter {
            $0.name.localizedCaseInsensitiveContains(searchText) ||
            $0.description.localizedCaseInsensitiveContains(searchText)
        }
    }

    func executeCommand(_ command: Command) {
        isExecuting = true
        Task {
            do {
                let result = try await configRepository.executeCommand(command.syntax)
                await MainActor.run {
                    self.result = command.successMessage
                    isExecuting = false
                    dismiss()
                }
            } catch {
                await MainActor.run {
                    self.error = error
                    isExecuting = false
                }
            }
        }
    }
}

struct CommandRow: View {
    let command: Command

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Image(systemName: command.icon)
                    .foregroundColor(.blue)
                    .frame(width: 24)

                Text(command.name)
                    .font(.body)
                    .fontWeight(.medium)

                Spacer()

                Text(command.syntax)
                    .font(.caption)
                    .foregroundColor(.secondary)
                    .padding(.horizontal, 8)
                    .padding(.vertical, 2)
                    .background(Color(.systemGray5))
                    .cornerRadius(4)
            }

            Text(command.description)
                .font(.caption)
                .foregroundColor(.secondary)
        }
        .padding(.vertical, 4)
    }
}

struct SearchBar: View {
    @Binding var text: String

    var body: some View {
        HStack {
            Image(systemName: "magnifyingglass")
                .foregroundColor(.gray)

            TextField("搜索命令...", text: $text)
                .textFieldStyle(.plain)

            if !text.isEmpty {
                Button(action: { text = "" }) {
                    Image(systemName: "xmark.circle.fill")
                        .foregroundColor(.gray)
                }
            }
        }
        .padding(8)
        .background(Color(.systemGray6))
        .cornerRadius(8)
    }
}

// MARK: - Command Models

struct Command: Identifiable {
    let id = UUID()
    let name: String
    let syntax: String
    let description: String
    let icon: String
    let successMessage: String
}

let commands = [
    Command(
        name: "设置推理级别",
        syntax: "/effort high",
        description: "设置 AI 推理努力程度（low/medium/high/xhigh/max/auto）",
        icon: "brain",
        successMessage: "推理级别已更新"
    ),
    Command(
        name: "切换模型",
        syntax: "/model opus",
        description: "切换 AI 模型（opus/sonnet/haiku）",
        icon: "cpu",
        successMessage: "模型已切换"
    ),
    Command(
        name: "快速模式",
        syntax: "/fast",
        description: "切换快速响应模式",
        icon: "bolt",
        successMessage: "快速模式已切换"
    ),
    Command(
        name: "自动模式",
        syntax: "/auto mode on",
        description: "开启/关闭自动执行模式",
        icon: "play.circle",
        successMessage: "自动模式已更新"
    ),
    Command(
        name: "压缩对话",
        syntax: "/compact",
        description: "压缩历史对话以释放空间",
        icon: "arrow.down.circle",
        successMessage: "对话已压缩"
    ),
    Command(
        name: "清空会话",
        syntax: "/clear",
        description: "清空当前会话内容",
        icon: "trash",
        successMessage: "会话已清空"
    ),
    Command(
        name: "显示帮助",
        syntax: "/help",
        description: "显示所有可用命令",
        icon: "questionmark.circle",
        successMessage: ""
    ),
    Command(
        name: "查看配置",
        syntax: "/config",
        description: "查看当前会话配置",
        icon: "gearshape",
        successMessage: ""
    ),
    Command(
        name: "工作流",
        syntax: "/workflows",
        description: "查看运行中的工作流",
        icon: "flowchart",
        successMessage: ""
    )
]
