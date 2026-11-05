import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { installDebugCapture } from './lib/debug-log';
import './index.css';

// Always capture console/promise errors into the debug ring buffer; the log
// panel itself stays hidden until ?debug=1.
installDebugCapture();

function revealApp() {
  const boot = document.getElementById('boot');
  if (!boot || boot.classList.contains('is-done')) return;
  boot.classList.add('is-done');
  window.setTimeout(() => boot.remove(), 450);
}

const container = document.getElementById('root');

if (container) {
  createRoot(container).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );

  // Fade the boot splash only after React has committed its first frame.
  // Fading on the script's first tick can reveal an empty page for a frame.
  const observer = new MutationObserver(() => {
    if (container.childElementCount === 0) return;
    observer.disconnect();
    requestAnimationFrame(() => requestAnimationFrame(revealApp));
  });
  observer.observe(container, { childList: true });

  if (container.childElementCount > 0) {
    observer.disconnect();
    requestAnimationFrame(() => requestAnimationFrame(revealApp));
  }

  // Never leave a stuck splash if the bundle half-loads.
  window.setTimeout(revealApp, 12000);
}
