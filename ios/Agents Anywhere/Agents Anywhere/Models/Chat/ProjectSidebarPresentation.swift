import Foundation

enum ProjectSidebarPresentation {
    static func visible(_ project: V2Project, filter: V2DeviceSessionFilter, sessions: [V2SessionMeta] = []) -> Bool {
        if project.manuallyCreated { return true }
        return sessions.contains { $0.projectId == project.id && governsProject($0, filter: filter) }
    }

    /// Whether a session keeps its project in the list.
    ///
    /// Pinning moves a row to the "Pinned" section, but the session still
    /// belongs to its project: hiding the project because its only session was
    /// pinned would take the project row and its "new session" button away.
    private static func governsProject(_ session: V2SessionMeta, filter: V2DeviceSessionFilter) -> Bool {
        switch filter {
        case .active: !session.archived
        case .archived: session.archived
        case .all: true
        }
    }
    private static func matches(_ session: V2SessionMeta, filter: V2DeviceSessionFilter) -> Bool {
        switch filter {
        case .active: !session.archived && !session.pinned
        case .archived: session.archived
        case .all: session.archived || !session.pinned
        }
    }
    static func sessions(_ values: [V2SessionMeta], projectID: String?, filter: V2DeviceSessionFilter) -> [V2SessionMeta] {
        SessionSidebarPresentation.sorted(values.filter { value in
            guard projectID == nil || value.projectId == projectID else { return false }
            return matches(value, filter: filter)
        })
    }
    static func unassignedSessions(_ values: [V2SessionMeta], projects: [V2Project], filter: V2DeviceSessionFilter) -> [V2SessionMeta] {
        let projectIDs = Set(projects.map(\.id))
        return sessions(values, projectID: nil, filter: filter).filter {
            $0.projectId.map { !projectIDs.contains($0) } ?? true
        }
    }
    static func projects(_ values: [V2Project], filter: V2DeviceSessionFilter, sessions: [V2SessionMeta] = []) -> [V2Project] {
        var activity: [String: TimeInterval] = [:]
        // One pass over sessions decides visibility for every project, instead
        // of scanning the whole session list once per project.
        var projectsWithMatches: Set<String> = []
        for session in sessions {
            guard let id = session.projectId else { continue }
            if governsProject(session, filter: filter) { projectsWithMatches.insert(id) }
            let time = [session.sortAt, session.lastActivityAt, session.createdAt].compactMap { $0 }.first { !$0.isEmpty }
            activity[id] = max(activity[id] ?? 0, timestamp(time))
        }
        // A duplicated id from the wire used to trap in
        // Dictionary(uniqueKeysWithValues:) and in the force unwraps below;
        // both now degrade to a stable fallback order instead of crashing.
        let order = Dictionary(values.map { project in
            let empty = project.manuallyCreated && (project.lastActivityAt?.isEmpty ?? true) && activity[project.id] == nil
                && project.activeSessionCount == 0 && project.sidebarSessionCounts.active == 0 && project.sidebarSessionCounts.archived == 0
            return (project.id, (empty: empty, activity: max(timestamp(project.lastActivityAt), activity[project.id] ?? 0), created: timestamp(project.createdAt)))
        }, uniquingKeysWith: { first, _ in first })
        return values.filter { $0.manuallyCreated || projectsWithMatches.contains($0.id) }.sorted {
            if $0.pinned != $1.pinned { return $0.pinned }
            guard let left = order[$0.id], let right = order[$1.id] else { return $0.id < $1.id }
            if left.empty != right.empty { return left.empty }
            if left.activity != right.activity { return left.activity > right.activity }
            if left.created != right.created { return left.created > right.created }
            let names = $0.name.localizedStandardCompare($1.name)
            return names == .orderedSame ? $0.id < $1.id : names == .orderedAscending
        }
    }

    private static func timestamp(_ value: String?) -> TimeInterval {
        V2Timestamp.seconds(value) ?? 0
    }
}
