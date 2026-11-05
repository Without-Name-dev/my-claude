"use client";

import { useState, useEffect } from "react";
import { useRouter } from "next/navigation";

interface Branch {
  branch_id: string;
  parent_branch_id: string;
  branch_point_item_id: string;
  name: string;
  created_at: number;
  last_activity_at?: number;
}

interface BranchTreeProps {
  sessionId: string;
  activeBranchId?: string;
  onBranchSwitch?: (branchId: string) => void;
}

export function BranchTree({ sessionId, activeBranchId, onBranchSwitch }: BranchTreeProps) {
  const [branches, setBranches] = useState<Branch[]>([]);
  const [loading, setLoading] = useState(true);
  const router = useRouter();

  useEffect(() => {
    loadBranches();
  }, [sessionId]);

  const loadBranches = async () => {
    try {
      const response = await fetch(`/api/v2/sessions/${sessionId}/branches`, {
        credentials: "include",
      });
      if (response.ok) {
        const data = await response.json();
        setBranches(data.branches || []);
      }
    } catch (error) {
      console.error("Failed to load branches:", error);
    } finally {
      setLoading(false);
    }
  };

  const switchBranch = async (branchId: string) => {
    try {
      const response = await fetch(`/api/v2/sessions/${sessionId}/branches/${branchId}/switch`, {
        method: "POST",
        credentials: "include",
      });
      if (response.ok) {
        onBranchSwitch?.(branchId);
        router.refresh();
      }
    } catch (error) {
      console.error("Failed to switch branch:", error);
    }
  };

  const deleteBranch = async (branchId: string) => {
    if (!confirm("Delete this branch? This cannot be undone.")) return;

    try {
      const response = await fetch(`/api/v2/sessions/${sessionId}/branches/${branchId}`, {
        method: "DELETE",
        credentials: "include",
      });
      if (response.ok) {
        await loadBranches();
        router.refresh();
      }
    } catch (error) {
      console.error("Failed to delete branch:", error);
    }
  };

  if (loading) {
    return <div className="p-4 text-sm text-gray-500">Loading branches...</div>;
  }

  const buildTree = () => {
    type TreeBranch = Branch & { children: TreeBranch[] };
    const map = new Map<string, TreeBranch>();
    const roots: TreeBranch[] = [];

    branches.forEach((branch) => {
      map.set(branch.branch_id, { ...branch, children: [] });
    });

    branches.forEach((branch) => {
      const node = map.get(branch.branch_id)!;
      if (branch.parent_branch_id && map.has(branch.parent_branch_id)) {
        map.get(branch.parent_branch_id)!.children.push(node);
      } else {
        roots.push(node);
      }
    });

    return roots;
  };

  const renderBranch = (branch: Branch & { children: any[] }, depth = 0) => {
    const isActive = branch.branch_id === activeBranchId;

    return (
      <div key={branch.branch_id} style={{ marginLeft: `${depth * 16}px` }}>
        <div
          className={`flex items-center justify-between p-2 rounded hover:bg-gray-100 cursor-pointer ${
            isActive ? "bg-blue-50 border-l-2 border-blue-500" : ""
          }`}
          onClick={() => switchBranch(branch.branch_id)}
        >
          <div className="flex-1">
            <div className="text-sm font-medium">
              {branch.name || `Branch ${branch.branch_id.slice(0, 8)}`}
            </div>
            <div className="text-xs text-gray-500">
              {new Date(branch.created_at).toLocaleString()}
            </div>
          </div>
          <button
            onClick={(e) => {
              e.stopPropagation();
              deleteBranch(branch.branch_id);
            }}
            className="text-red-500 hover:text-red-700 text-sm px-2"
          >
            Delete
          </button>
        </div>
        {branch.children.map((child) => renderBranch(child, depth + 1))}
      </div>
    );
  };

  const tree = buildTree();

  return (
    <div className="border rounded-lg p-4 bg-white">
      <div className="flex items-center justify-between mb-4">
        <h3 className="text-lg font-semibold">Conversation Branches</h3>
      </div>

      <div
        className={`p-2 rounded mb-2 cursor-pointer hover:bg-gray-100 ${
          !activeBranchId || activeBranchId === "main" ? "bg-blue-50 border-l-2 border-blue-500" : ""
        }`}
        onClick={() => switchBranch("main")}
      >
        <div className="text-sm font-medium">Main Branch</div>
      </div>

      {tree.length === 0 ? (
        <div className="text-sm text-gray-500 p-2">No branches yet</div>
      ) : (
        <div className="space-y-1">{tree.map((branch) => renderBranch(branch))}</div>
      )}
    </div>
  );
}
