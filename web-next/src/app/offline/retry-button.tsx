"use client";

/** Reloads the app shell after connectivity returns. Kept as its own client
 * component so the page can stay a Server Component and keep its metadata. */
export function OfflineRetryButton() {
  return (
    <button
      onClick={() => window.location.reload()}
      className="rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90"
    >
      Retry
    </button>
  );
}
