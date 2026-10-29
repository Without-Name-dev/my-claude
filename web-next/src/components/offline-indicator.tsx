"use client";

import { useOfflineStatus } from "@/lib/use-offline";
import { RiWifiOffLine, RiSignalWifiErrorLine } from "@remixicon/react";

export function OfflineIndicator() {
  const { isOnline, isSlowConnection } = useOfflineStatus();

  if (isOnline && !isSlowConnection) {
    return null;
  }

  return (
    <div className="fixed bottom-4 right-4 z-50 flex items-center gap-2 rounded-lg bg-amber-500/90 px-4 py-2 text-sm font-medium text-white shadow-lg backdrop-blur-sm">
      {!isOnline ? (
        <>
          <RiWifiOffLine className="h-4 w-4" />
          <span>Offline - Using cached data</span>
        </>
      ) : (
        <>
          <RiSignalWifiErrorLine className="h-4 w-4" />
          <span>Slow connection</span>
        </>
      )}
    </div>
  );
}
