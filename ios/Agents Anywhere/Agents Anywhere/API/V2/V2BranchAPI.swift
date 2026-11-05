//
//  V2BranchAPI.swift
//  Agents Anywhere
//
//  Branch management API calls
//

import Foundation

extension V2API {

    /// Create a new conversation branch from a message
    func createBranch(sessionId: String, itemId: String, name: String?) async throws -> CreateBranchResponse {
        let request = CreateBranchRequest(itemId: itemId, name: name)
        let data = try JSONEncoder().encode(request)

        return try await post(
            path: "/sessions/\(sessionId)/branches",
            body: data,
            responseType: CreateBranchResponse.self
        )
    }

    /// List all branches for a session
    func listBranches(sessionId: String) async throws -> [ConversationBranch] {
        let response: BranchListResponse = try await get(
            path: "/sessions/\(sessionId)/branches"
        )
        return response.branches
    }

    /// Switch to a different branch
    func switchBranch(sessionId: String, branchId: String) async throws {
        let _: EmptyResponse = try await post(
            path: "/sessions/\(sessionId)/branches/\(branchId)/switch",
            body: Data(),
            responseType: EmptyResponse.self
        )
    }

    /// Get timeline items for a specific branch
    func getBranchTimeline(sessionId: String, branchId: String) async throws -> [V2TimelineItem] {
        struct TimelineResponse: Codable {
            let items: [V2TimelineItem]
        }

        let response: TimelineResponse = try await get(
            path: "/sessions/\(sessionId)/branches/\(branchId)/timeline"
        )
        return response.items
    }

    /// Delete a branch
    func deleteBranch(sessionId: String, branchId: String) async throws {
        let _: EmptyResponse = try await delete(
            path: "/sessions/\(sessionId)/branches/\(branchId)"
        )
    }
}

private struct EmptyResponse: Codable {}
