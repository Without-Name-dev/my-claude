/**
 * Service Worker 注册和管理
 */

export async function registerServiceWorker(): Promise<ServiceWorkerRegistration | null> {
  if (typeof window === "undefined" || !("serviceWorker" in navigator)) {
    return null;
  }

  try {
    const registration = await navigator.serviceWorker.register("/sw.js", {
      scope: "/",
    });

    registration.addEventListener("updatefound", () => {
      const newWorker = registration.installing;
      if (newWorker) {
        newWorker.addEventListener("statechange", () => {
          if (newWorker.state === "installed" && navigator.serviceWorker.controller) {
            // 新版本可用，提示用户刷新
            if (window.confirm("New version available. Reload to update?")) {
              window.location.reload();
            }
          }
        });
      }
    });

    return registration;
  } catch (error) {
    console.error("Service Worker registration failed:", error);
    return null;
  }
}

export async function unregisterServiceWorker(): Promise<boolean> {
  if (typeof window === "undefined" || !("serviceWorker" in navigator)) {
    return false;
  }

  try {
    const registration = await navigator.serviceWorker.getRegistration();
    if (registration) {
      return await registration.unregister();
    }
    return false;
  } catch (error) {
    console.error("Service Worker unregistration failed:", error);
    return false;
  }
}
