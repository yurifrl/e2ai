# Mobile Tooling Bootstrap (plan 04-01, T2)

Installed 2026-09-20 on Yuri's dev Mac (arm64, Darwin 25.6.0). Install-and-record only —
no emulator/simulator booted, no code changes.

## Environment exports (required)

`~/.zshrc` (and mirrored where relevant for `.bash_profile` for maestro):

```sh
export JAVA_HOME=/opt/homebrew/opt/openjdk/libexec/openjdk.jdk/Contents/Home
export ANDROID_HOME=/opt/homebrew/share/android-commandlinetools
export PATH=$PATH:$ANDROID_HOME/emulator:$ANDROID_HOME/platform-tools:$JAVA_HOME/bin
export PATH=$PATH:/opt/homebrew/opt/openjdk/bin        # openjdk (brew formula)
export PATH=$PATH:$HOME/.maestro/bin                  # added by the maestro installer
```

Without `JAVA_HOME`, `/usr/bin/java` fails ("Unable to locate a Java Runtime") and
`sdkmanager`/`maestro` will not start.

## Pins

| Tool | Version | Provenance / location |
|---|---|---|
| Xcode | 27.0 (27A266a) | pre-existing |
| iOS runtimes | 26.4 (23E244), 26.5 (23F77) — both Ready | pre-existing (`xcrun simctl runtime list`) |
| OpenJDK | 27 (java.version 27, 2026-09-15) | `brew install openjdk` (formula, no sudo; temurin cask rejected — requires sudo pkg installer) → `/opt/homebrew/opt/openjdk` |
| Android cmdline-tools | 22.0 | `brew install --cask android-commandlinetools` (SDK build 15859902) |
| Android SDK root | — | `/opt/homebrew/share/android-commandlinetools` (= `ANDROID_HOME`) |
| platform-tools (adb) | adb 1.0.41 / 37.0.1-15733141 | `sdkmanager "platform-tools"` |
| emulator | 37.1.11 (build_id 15917651) | `sdkmanager "emulator"` |
| system-image | `system-images;android-35;google_apis;arm64-v8a` rev 9 | `sdkmanager` — latest stable google_apis arm64 image in the requested API 34–35 range (android-36/36-ext18 exist but out of scope) |
| Maestro | 2.10.0 | official installer script (curl maestro.mobile.dev) → `~/.maestro/bin/maestro` |
| idb-companion | 1.6.1 (build Sep 18 2026) | `brew tap facebook/fb` + `brew install facebook/fb/idb-companion` (tap had to be trusted: `brew trust facebook/fb`); binary linked as `idb_companion` in `/opt/homebrew/bin` |

All SDK packages installed with `yes | sdkmanager --licenses` accepted up front.

## Verified commands

- `maestro --version` → `2.10.0`
- `adb version` → 1.0.41 / 37.0.1-15733141
- `emulator -list-avds` → exits 0, no AVDs created (per plan: no emulator boots)

## Notes

- `sdkmanager` prints a deprecation warning pointing to the `android` CLI in
  `cmdline-tools/latest/bin`; sdkmanager still works.
- No AVDs were created and no emulator was launched (T2 scope). Creating/launching
  AVDs is deferred to later tasks.
