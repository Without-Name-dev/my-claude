import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { installDebugCapture } from './lib/debug-log';
import './index.css';

// Always capture console/promise errors into the debug ring buffer; the log
// panel itself stays hidden until ?debug=1.
installDebugCapture();

let revealed = false;

/**
 * The splash used to vanish as soon as React committed — on a fast connection
 * that read as a flicker rather than a launch. Keep it up for at least this
 * long; index.html stamps the first paint so time spent loading HTML counts.
 */
const BOOT_MIN_VISIBLE_MS = 1500;

function revealApp() {
  if (revealed) return;
  revealed = true;
  const startedAt = Number(document.documentElement.dataset.bootAt) || Date.now();
  const remaining = Math.max(0, BOOT_MIN_VISIBLE_MS - (Date.now() - startedAt));
  const finish = () => window.dispatchEvent(new Event('ccremote:boot-done'));
  // One frame for React's commit, one for the browser to paint it, then fade.
  window.setTimeout(() => {
    requestAnimationFrame(() => {
      requestAnimationFrame(() => {
        // Inputs mount under the splash; re-enable touch and focus only now so
        // autofocus cannot raise the keyboard while the splash is still up.
        document.getElementById('root')?.removeAttribute('inert');
        const boot = document.getElementById('boot');
        if (!boot) {
          finish();
          return;
        }
        boot.classList.add('is-done');
        window.setTimeout(() => {
          boot.remove();
          finish();
        }, 450);
      });
    });
  }, remaining);
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
  // The splash is decorative for sighted users; announce it for VoiceOver.
  const boot = document.getElementById('boot');
  if (boot) {
    boot.setAttribute('role', 'status');
    boot.setAttribute('aria-live', 'polite');
  }
  // Never leave a stuck splash if the bundle half-loads.
  window.setTimeout(revealApp, 12000);
}
