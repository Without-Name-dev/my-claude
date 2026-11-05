import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { installDebugCapture } from './lib/debug-log';
import './index.css';

// Always capture console/promise errors into the debug ring buffer; the log
// panel itself stays hidden until ?debug=1.
installDebugCapture();

let revealed = false;

function revealApp() {
  if (revealed) return;
  revealed = true;
  // One frame for React's commit, one for the browser to paint it, then fade.
  requestAnimationFrame(() => {
    requestAnimationFrame(() => {
      const boot = document.getElementById('boot');
      if (!boot) return;
      boot.classList.add('is-done');
      window.setTimeout(() => boot.remove(), 450);
    });
  });
}

const container = document.getElementById('root');

if (container) {
  createRoot(container).render(
    <StrictMode>
      <App />
    </StrictMode>,
  );

  // App fires this once the real (lazily loaded) screen is committed.
  window.addEventListener('ccremote:ready', revealApp, { once: true });
  // Never leave a stuck splash if the bundle half-loads.
  window.setTimeout(revealApp, 12000);
}
