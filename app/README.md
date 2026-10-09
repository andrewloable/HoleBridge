# holebridge

A new Flutter project.

## Getting Started

This project is a starting point for a Flutter application.

A few resources to get you started if this is your first Flutter project:

- [Learn Flutter](https://docs.flutter.dev/get-started/learn-flutter)
- [Write your first Flutter app](https://docs.flutter.dev/get-started/codelab)
- [Flutter learning resources](https://docs.flutter.dev/reference/learning-resources)

For help getting started with Flutter development, view the
[online documentation](https://docs.flutter.dev/), which offers tutorials,
samples, guidance on mobile development, and a full API reference.

## Android release builds

Build the release APK with the Flutter tool, not with `./gradlew assembleRelease` on its own:

```sh
cd app
flutter build apk --release
```

Flutter rewrites `android/app/src/main/java/io/flutter/plugins/GeneratedPluginRegistrant.java` on
each build. A release build leaves out the dev-only plugin `integration_test`, so the file compiles.
`flutter pub get` rewrites it with that plugin included, and a raw `./gradlew assembleRelease` run
after that fails with "package dev.flutter.plugins.integration_test does not exist" until
`flutter build apk --release` runs again. `./gradlew assembleDebug` is not affected.

R8 keeps the VPN JNI binding `app.holebridge.holebridge.vpn.TunNative` and its native methods, as
set in `android/app/proguard-rules.pro`. The native library looks them up by name.
