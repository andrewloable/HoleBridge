// The engine lifecycle per platform and VPN mode (docs/architecture.md#sessions-and-reconnects,
// docs/architecture.md#platforms, docs/architecture.md#vpn-mode-android-and-ios).
import 'package:flutter/widgets.dart';

import 'engine_host.dart';

/// The platform families whose engine lifecycle differs. Android covers phones and TVs, which run the
/// same APK.
enum Platform { desktop, android, ios }

/// VPN mode as the lifecycle sees it: true while VPN mode is on. The VPN controller
/// (HoleBridge-hb5.5.9) implements it; the tests fake it.
abstract interface class VpnState {
  bool get enabled;
}

/// Pauses and resumes the engine as the app goes to the background and returns.
///
/// Desktop keeps the engine running while the window is hidden (tray). Android with VPN mode on keeps
/// it running in the background. Android with VPN mode off and iOS suspend it when the app goes to the
/// background and resume it when the app returns: listeners exist only while the app is open. A resume
/// does not reconnect; a session opens on demand, when a tile is tapped.
///
/// The constructor does not register the object. The app wiring adds it with
/// WidgetsBinding.instance.addObserver once the engine is started, and removes it with
/// WidgetsBinding.instance.removeObserver when the app tears down.
///
/// Host calls run one after the other, in the order the app state changed. A failure is reported
/// through FlutterError, not thrown.
class EngineLifecycle with WidgetsBindingObserver {
  EngineLifecycle(this._host, this._vpn, this._platform);

  final EngineHost _host;
  final VpnState _vpn;
  final Platform _platform;

  /// True from the moment a suspend is requested until the matching resume is requested. The worklet is
  /// called only when this changes, so a repeated paused or resumed state never reaches it.
  bool _suspended = false;

  /// The host calls so far, each queued after the one before. A resume waits for a suspend still in
  /// flight, because the worklet sets its state to suspended only after the suspend returns, and a
  /// resume does nothing unless it is suspended.
  Future<void> _queue = Future<void>.value();

  @override
  void didChangeAppLifecycleState(AppLifecycleState state) {
    switch (state) {
      case AppLifecycleState.paused:
        if (_suspended || !_suspendsInBackground) return;
        _suspended = true;
        _enqueue(_host.suspend, 'suspends');
      case AppLifecycleState.resumed:
        // Resume whatever the VPN state is now: a suspended engine must never be left suspended.
        if (!_suspended) return;
        _suspended = false;
        _enqueue(_host.resume, 'resumes');
      case AppLifecycleState.inactive:
      case AppLifecycleState.hidden:
      case AppLifecycleState.detached:
        return;
    }
  }

  /// Runs [call] after everything queued before it. A failure, whether thrown now or completed with an
  /// error later, is reported through FlutterError and never escapes.
  void _enqueue(Future<void> Function() call, String what) {
    _queue = _queue.then((_) async {
      try {
        await call();
      } catch (error, stack) {
        FlutterError.reportError(FlutterErrorDetails(
          exception: error,
          stack: stack,
          library: 'holebridge engine lifecycle',
          context: ErrorDescription('while the engine $what'),
        ));
      }
    });
  }

  /// Whether the engine is suspended when the app goes to the background: never on desktop, only with
  /// VPN mode off on Android, and always on iOS.
  bool get _suspendsInBackground => switch (_platform) {
        Platform.desktop => false,
        Platform.android => !_vpn.enabled,
        Platform.ios => true,
      };
}
