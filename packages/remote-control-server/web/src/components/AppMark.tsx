/**
 * The app mark: an orange blocky character (body, two black eyes, side arms,
 * four legs). Same artwork as the iOS app icon and launch screen, so the first
 * frame after launch matches the icon.
 *
 * Deliberately static — the launch/loading screens used to pulse, which read as
 * "still loading forever" in a WKWebView.
 */
export function AppMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 1024 1024" className={className} role="img" aria-label="Claude Remote">
      <g fill="#DF7C57">
        <rect x="170" y="150" width="684" height="470" rx="18" />
        <rect x="60" y="348" width="120" height="108" />
        <rect x="844" y="348" width="120" height="108" />
        <rect x="245" y="612" width="62" height="152" />
        <rect x="380" y="612" width="62" height="152" />
        <rect x="582" y="612" width="62" height="152" />
        <rect x="717" y="612" width="62" height="152" />
      </g>
      <g fill="#161514">
        <rect x="300" y="252" width="58" height="130" rx="10" />
        <rect x="666" y="252" width="58" height="130" rx="10" />
      </g>
    </svg>
  );
}
