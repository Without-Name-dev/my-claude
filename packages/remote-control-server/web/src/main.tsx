import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { installDebugCapture } from './lib/debug-log';
import './index.css';

// Always capture console/promise errors into the debug ring buffer; the log
// panel itself stays hidden until ?debug=1.
installDebugCapture();

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
