// The app's entry point. It runs the real wiring: the platform engine host, the secure store and the app
// root (src/app.dart), which builds the controller, the engine lifecycle and the screens.
import 'dart:io' as io;

import 'package:flutter/widgets.dart';

import 'src/app.dart';
import 'src/engine/engine_host.dart';
import 'src/engine/lifecycle.dart';
import 'src/store/host_store.dart';
import 'src/store/secure_backend.dart';

void main() {
  runApp(
    HoleBridgeApp(
      host: EngineHost(),
      store: HostStore(SecureStorageBackend()),
      platform: _runningPlatform(),
    ),
  );
}

/// The platform family this run is on, for the engine lifecycle. Android covers phones and TVs, which
/// run the same APK. Desktop covers macOS, Linux and Windows.
Platform _runningPlatform() {
  if (io.Platform.isAndroid) return Platform.android;
  if (io.Platform.isIOS) return Platform.ios;
  return Platform.desktop;
}
