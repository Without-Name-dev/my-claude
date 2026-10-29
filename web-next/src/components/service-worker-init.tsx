"use client";

import { useEffect } from "react";
import { registerServiceWorker } from "@/lib/service-worker";

export function ServiceWorkerInit() {
  useEffect(() => {
    if (typeof window !== "undefined" && "serviceWorker" in navigator) {
      // Background sync is not registered: the server only accepts Bearer
      // authentication, which a service worker cannot supply, so every sync
      // attempt could only 401. Re-enable together with a token handoff.
      registerServiceWorker();
    }
  }, []);

  return null;
}
