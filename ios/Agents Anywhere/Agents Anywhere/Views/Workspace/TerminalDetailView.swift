import SwiftUI

struct TerminalDetailView: View {
    let terminal: V2Terminal

    @State private var outputText: String = ""
    @State private var isLoadingOutput = false
    @Environment(\.colorScheme) private var colorScheme

    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 20) {
                // 终端信息卡片
                terminalInfoSection

                // 输出预览
                outputPreviewSection
            }
            .padding()
        }
        .navigationTitle(terminal.label.isEmpty ? "终端详情" : terminal.label)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .primaryAction) {
                Menu {
                    Button {
                        copyWorkingDirectory()
                    } label: {
                        Label("复制工作目录", systemImage: "doc.on.doc")
                    }

                    if terminal.pid > 0 {
                        Button {
                            copyPID()
                        } label: {
                            Label("复制 PID", systemImage: "number")
                        }
                    }
                } label: {
                    Image(systemName: "ellipsis.circle")
                }
            }
        }
        .task {
            await loadOutput()
        }
    }

    private var terminalInfoSection: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("终端信息")
                .font(.headline)

            GroupBox {
                VStack(spacing: 12) {
                    InfoRow(label: "状态", value: terminal.statusDisplay, icon: "circle.fill", iconColor: statusColor)

                    Divider()

                    InfoRow(label: "Shell", value: terminal.shell, icon: "terminal.fill")

                    if terminal.pid > 0 {
                        Divider()
                        InfoRow(label: "进程 ID", value: String(terminal.pid), icon: "number")
                    }

                    if let exitCode = terminal.exitCode {
                        Divider()
                        InfoRow(label: "退出码", value: String(exitCode), icon: "xmark.circle")
                    }

                    Divider()

                    InfoRow(label: "尺寸", value: "\(terminal.cols) × \(terminal.rows)", icon: "rectangle")

                    Divider()

                    VStack(alignment: .leading, spacing: 4) {
                        HStack {
                            Image(systemName: "folder.fill")
                                .foregroundStyle(.secondary)
                                .frame(width: 20)
                            Text("工作目录")
                                .foregroundStyle(.secondary)
                        }
                        .font(.subheadline)

                        Text(terminal.workingDirectory)
                            .font(.system(.body, design: .monospaced))
                            .textSelection(.enabled)
                    }

                    Divider()

                    InfoRow(
                        label: "更新时间",
                        value: formatFullTimestamp(terminal.updatedAt),
                        icon: "clock"
                    )
                }
                .padding(8)
            }
        }
    }

    private var outputPreviewSection: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Text("输出摘要")
                    .font(.headline)

                Spacer()

                if terminal.scrollbackSeq > 0 {
                    Text("序列: \(terminal.scrollbackSeq)")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
            }

            GroupBox {
                if isLoadingOutput {
                    HStack {
                        Spacer()
                        ProgressView()
                        Spacer()
                    }
                    .padding()
                } else if outputText.isEmpty {
                    Text("暂无输出")
                        .foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, alignment: .center)
                        .padding()
                } else {
                    ScrollView(.horizontal, showsIndicators: true) {
                        Text(outputText)
                            .font(.system(.caption, design: .monospaced))
                            .textSelection(.enabled)
                            .padding(8)
                    }
                    .frame(maxHeight: 300)
                    .background(terminalBackgroundColor)
                    .clipShape(RoundedRectangle(cornerRadius: 8))
                }
            }

            Text("移动端仅支持查看，不支持输入。请在桌面端使用完整终端功能。")
                .font(.caption)
                .foregroundStyle(.secondary)
                .padding(.horizontal, 4)
        }
    }

    private var statusColor: Color {
        switch terminal.status {
        case "starting": return .yellow
        case "running": return .green
        case "exited": return .gray
        default: return .gray
        }
    }

    private var terminalBackgroundColor: Color {
        colorScheme == .dark ? Color(white: 0.1) : Color(white: 0.95)
    }

    private func loadOutput() async {
        isLoadingOutput = true

        // Note: 这里需要从服务端获取终端输出
        // 暂时显示占位信息
        await Task.sleep(500_000_000) // 0.5 秒
        outputText = "终端输出预览功能开发中...\n\n当前终端状态: \(terminal.statusDisplay)"

        isLoadingOutput = false
    }

    private func copyWorkingDirectory() {
        UIPasteboard.general.string = terminal.workingDirectory
    }

    private func copyPID() {
        UIPasteboard.general.string = String(terminal.pid)
    }

    private func formatFullTimestamp(_ timestamp: Int64) -> String {
        let date = Date(timeIntervalSince1970: TimeInterval(timestamp) / 1000)
        let formatter = DateFormatter()
        formatter.dateStyle = .medium
        formatter.timeStyle = .medium
        return formatter.string(from: date)
    }
}

struct InfoRow: View {
    let label: String
    let value: String
    let icon: String
    var iconColor: Color?

    var body: some View {
        HStack {
            Image(systemName: icon)
                .foregroundStyle(iconColor ?? .secondary)
                .frame(width: 20)

            Text(label)
                .foregroundStyle(.secondary)

            Spacer()

            Text(value)
                .fontWeight(.medium)
        }
        .font(.subheadline)
    }
}
