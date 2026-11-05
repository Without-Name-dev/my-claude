"use client";

import { useState } from "react";

interface CreateBranchModalProps {
  sessionId: string;
  itemId: string;
  onClose: () => void;
  onCreated?: () => void;
}

export function CreateBranchModal({ sessionId, itemId, onClose, onCreated }: CreateBranchModalProps) {
  const [name, setName] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");

  const handleCreate = async () => {
    setLoading(true);
    setError("");

    try {
      const response = await fetch(`/api/v2/sessions/${sessionId}/branches`, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ item_id: itemId, name }),
      });

      if (response.ok) {
        onCreated?.();
        onClose();
      } else {
        const data = await response.json();
        setError(data.error || "Failed to create branch");
      }
    } catch (err) {
      setError("Network error");
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="fixed inset-0 bg-black bg-opacity-50 flex items-center justify-center z-50">
      <div className="bg-white rounded-lg p-6 max-w-md w-full mx-4">
        <h2 className="text-xl font-semibold mb-4">Create Branch</h2>

        <div className="mb-4">
          <label className="block text-sm font-medium mb-2">Branch Name (optional)</label>
          <input
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g., Alternative approach"
            className="w-full px-3 py-2 border rounded-md focus:outline-none focus:ring-2 focus:ring-blue-500"
            disabled={loading}
          />
        </div>

        {error && (
          <div className="mb-4 p-3 bg-red-50 border border-red-200 rounded text-red-700 text-sm">
            {error}
          </div>
        )}

        <div className="flex justify-end gap-2">
          <button
            onClick={onClose}
            disabled={loading}
            className="px-4 py-2 text-gray-700 hover:bg-gray-100 rounded"
          >
            Cancel
          </button>
          <button
            onClick={handleCreate}
            disabled={loading}
            className="px-4 py-2 bg-blue-500 text-white rounded hover:bg-blue-600 disabled:opacity-50"
          >
            {loading ? "Creating..." : "Create Branch"}
          </button>
        </div>
      </div>
    </div>
  );
}

interface MessageActionsProps {
  sessionId: string;
  itemId: string;
  onBranchCreated?: () => void;
}

export function MessageActions({ sessionId, itemId, onBranchCreated }: MessageActionsProps) {
  const [showBranchModal, setShowBranchModal] = useState(false);

  return (
    <>
      <button
        onClick={() => setShowBranchModal(true)}
        className="text-sm text-gray-600 hover:text-blue-600 flex items-center gap-1"
        title="Create branch from this message"
      >
        <svg
          className="w-4 h-4"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            strokeLinecap="round"
            strokeLinejoin="round"
            strokeWidth={2}
            d="M8 7h12m0 0l-4-4m4 4l-4 4m0 6H4m0 0l4 4m-4-4l4-4"
          />
        </svg>
        Branch
      </button>

      {showBranchModal && (
        <CreateBranchModal
          sessionId={sessionId}
          itemId={itemId}
          onClose={() => setShowBranchModal(false)}
          onCreated={() => {
            onBranchCreated?.();
            setShowBranchModal(false);
          }}
        />
      )}
    </>
  );
}
