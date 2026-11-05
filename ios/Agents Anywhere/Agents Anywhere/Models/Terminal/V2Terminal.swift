import Foundation

// MARK: - Terminal Models

typealias V2TerminalID = String

struct V2Terminal: Codable, Hashable, Identifiable {
    let id: V2TerminalID
    let connectorId: String
    let userId: String
    let sessionId: String
    let label: String
    let root: String
    let cwd: String
    let shell: String
    let cols: Int
    let rows: Int
    let status: String // "starting" | "running" | "exited"
    let exitCode: Int?
    let pid: Int
    let scrollbackSeq: Int64
    let createdAt: Int64
    let updatedAt: Int64

    var isActive: Bool {
        status == "starting" || status == "running"
    }

    var statusDisplay: String {
        switch status {
        case "starting": return "启动中"
        case "running": return "运行中"
        case "exited": return "已退出"
        default: return status
        }
    }

    var workingDirectory: String {
        cwd.isEmpty ? root : cwd
    }
}

struct V2TerminalListResponse: Codable {
    let terminals: [V2Terminal]
}

struct V2TerminalOutput: Codable {
    let terminalId: String
    let seq: UInt64
    let data: String // base64 encoded
}

struct V2TerminalState: Codable {
    let terminalId: String
    let running: Bool
    let seq: UInt64?
    let data: String? // base64 encoded scrollback
}

struct V2TerminalError: Codable {
    let terminalId: String
    let code: String
    let message: String
}
