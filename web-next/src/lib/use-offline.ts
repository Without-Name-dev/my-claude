import { useEffect, useState } from "react";

export type NetworkStatus = {
  isOnline: boolean;
  isSlowConnection: boolean;
};

export function useOfflineStatus(): NetworkStatus {
  const [isOnline, setIsOnline] = useState(
    typeof navigator !== "undefined" ? navigator.onLine : true
  );
  const [isSlowConnection, setIsSlowConnection] = useState(false);

  useEffect(() => {
    const handleOnline = () => setIsOnline(true);
    const handleOffline = () => setIsOnline(false);

    window.addEventListener("online", handleOnline);
    window.addEventListener("offline", handleOffline);

    // 检测慢速连接
    if ("connection" in navigator) {
      const connection = (navigator as any).connection;
      const updateConnectionStatus = () => {
        const effectiveType = connection?.effectiveType;
        setIsSlowConnection(effectiveType === "slow-2g" || effectiveType === "2g");
      };
      updateConnectionStatus();
      connection?.addEventListener("change", updateConnectionStatus);

      return () => {
        window.removeEventListener("online", handleOnline);
        window.removeEventListener("offline", handleOffline);
        connection?.removeEventListener("change", updateConnectionStatus);
      };
    }

    return () => {
      window.removeEventListener("online", handleOnline);
      window.removeEventListener("offline", handleOffline);
    };
  }, []);

  return { isOnline, isSlowConnection };
}
