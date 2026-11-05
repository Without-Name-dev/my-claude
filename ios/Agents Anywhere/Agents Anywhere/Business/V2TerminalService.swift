import Foundation

struct V2TerminalService {
    let terminalAPI: any V2TerminalAPIProtocol

    func listTerminals(connectorId: V2ConnectorID) async throws -> [V2Terminal] {
        let response = try await terminalAPI.listTerminals(connectorId: connectorId)
        return response.terminals
    }
}
