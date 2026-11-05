# iOS shell (Capacitor)

A thin WKWebView shell for the RCS mobile web UI. It loads
`http://8.166.138.207/code/` directly, so the phone and the browser share the
same page and the same `/web` API.

The goal is a downloadable unsigned IPA that can be signed with Sideloadly.
There is no native business logic here.

## Build locally (macOS only)

```bash
npm install
npx cap add ios
npx cap sync ios
npx cap open ios
```

## CI

`.github/workflows/ios-unsigned-ipa.yml` runs on `macos-26`, adds the iOS
platform, builds an unsigned archive, packages `ClaudeRemote-unsigned.ipa`,
and publishes it as the `ios-latest` GitHub release.

Bundle ID is fixed to `app.jade5197.gypsum9420` so reinstalling upgrades the
existing app instead of creating a second one.

## Notes

- The app needs the server reachable over HTTP; the workflow adds the ATS
  exception to the generated `Info.plist`.
- Change `server.url` in `capacitor.config.ts` when the public address changes.

The shell is deployed from CI; see `.github/workflows/ios-unsigned-ipa.yml`.
