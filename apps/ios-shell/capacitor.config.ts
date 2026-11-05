import type { CapacitorConfig } from '@capacitor/cli'

/**
 * Thin native shell around the mobile web UI.
 *
 * The app loads the RCS web UI from the deployed server instead of bundling a
 * copy of it, so the phone always gets the same page (and the same /web API)
 * as the browser. The bundled `www` page is only a fallback for launch errors.
 */
const config: CapacitorConfig = {
  appId: 'app.jade5197.ccremote',
  appName: 'Claude Remote',
  webDir: 'www',
  server: {
    url: 'http://8.166.138.207/code/',
    cleartext: true,
    allowNavigation: ['8.166.138.207'],
  },
  ios: {
    // The web page owns the safe-area insets and the keyboard layout.
    contentInset: 'never',
    backgroundColor: '#efeee9',
  },
}

export default config
