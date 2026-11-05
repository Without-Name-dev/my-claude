//
//  ConversationBranch.swift
//  Agents Anywhere
//
//  Created for message branching feature
//

import Foundation

struct ConversationBranch: Codable, Identifiable, Hashable {
    let branchId: String
    let parentBranchId: String?
    let branchPointItemId: String
    let branchPointOrderSeq: Int64
    let name: String?
    let createdAt: Date
    let updatedAt: Date
    let lastActivityAt: Date?

    var id: String { branchId }

    enum CodingKeys: String, CodingKey {
        case branchId = "branch_id"
        case parentBranchId = "parent_branch_id"
        case branchPointItemId = "branch_point_item_id"
        case branchPointOrderSeq = "branch_point_order_seq"
        case name
        case createdAt = "created_at"
        case updatedAt = "updated_at"
        case lastActivityAt = "last_activity_at"
    }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        branchId = try container.decode(String.self, forKey: .branchId)
        parentBranchId = try container.decodeIfPresent(String.self, forKey: .parentBranchId)
        branchPointItemId = try container.decode(String.self, forKey: .branchPointItemId)
        branchPointOrderSeq = try container.decode(Int64.self, forKey: .branchPointOrderSeq)
        name = try container.decodeIfPresent(String.self, forKey: .name)

        let createdAtMs = try container.decode(Int64.self, forKey: .createdAt)
        createdAt = Date(timeIntervalSince1970: Double(createdAtMs) / 1000.0)

        let updatedAtMs = try container.decode(Int64.self, forKey: .updatedAt)
        updatedAt = Date(timeIntervalSince1970: Double(updatedAtMs) / 1000.0)

        if let activityMs = try container.decodeIfPresent(Int64.self, forKey: .lastActivityAt) {
            lastActivityAt = Date(timeIntervalSince1970: Double(activityMs) / 1000.0)
        } else {
            lastActivityAt = nil
        }
    }

    var displayName: String {
        if let name = name, !name.isEmpty {
            return name
        }
        return "Branch \(branchId.prefix(8))"
    }
}

struct BranchListResponse: Codable {
    let branches: [ConversationBranch]
}

struct CreateBranchRequest: Codable {
    let itemId: String
    let name: String?

    enum CodingKeys: String, CodingKey {
        case itemId = "item_id"
        case name
    }
}

struct CreateBranchResponse: Codable {
    let branchId: String
    let branch: ConversationBranch

    enum CodingKeys: String, CodingKey {
        case branchId = "branch_id"
        case branch
    }
}
