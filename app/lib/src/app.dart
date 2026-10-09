// The app root: starts the engine, registers the engine lifecycle once the engine runs, and shows the
// screens (docs/cli.md#the-app, docs/architecture.md#sessions-and-reconnects). It holds no business logic:
// AppController owns every engine request, and the screens take what they need from it.
import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart' show PlatformException;

import 'app_controller.dart';
import 'engine/engine_host.dart';
import 'engine/ipc_codec.dart' show IpcDesync;
import 'engine/lifecycle.dart';
import 'store/host_store.dart';
import 'tv/tv_focus.dart';
import 'ui/add_host_screen.dart';
import 'ui/diagnostics_screen.dart';
import 'ui/error_view.dart';
import 'ui/home_screen.dart';
import 'ui/host_screen.dart';
import 'ui/settings_screen.dart';
import 'vpn/vpn_mode_control.dart';

/// The app version (app/pubspec.yaml), the engine's package version (app/engine/package.json, the only
/// engine version there is) and the wire protocol version (docs/architecture.md, wire protocol v1).
const _appVersion = '1.0.0';
const _engineVersion = '0.0.0';
const _protocolVersion = '1';

/// The code of an engine that did not start (spec/errors.json).
const _engineDownCode = 'HB-ENGINE-DOWN';

/// VPN mode until the VPN controller exists. It is off, and turning it on is refused, which the settings
/// screen shows as a notice.
// ponytail: stub until HoleBridge-hb5.5.9 (VpnController) lands; swap it in where this is passed.
class _VpnNotReady implements VpnState, VpnModeControl {
  const _VpnNotReady();

  @override
  bool get enabled => false;

  @override
  Future<void> enable() async {
    throw PlatformException(code: 'vpn-not-ready');
  }

  @override
  Future<void> disable() async {}
}

/// Where the engine is: starting, running, or failed to start.
enum _Engine { starting, running, failed }

/// The app root. The app passes the real [host], [store] and [platform] (lib/main.dart); tests pass fakes.
///
/// The controller and the lifecycle are built here. The engine starts in initState. The lifecycle
/// observer and the controller listener are added once the engine runs, and removed on teardown. A
/// failed start, or a restart that fails after a crash, shows HB-ENGINE-DOWN with a retry.
class HoleBridgeApp extends StatefulWidget {
  const HoleBridgeApp({
    super.key,
    required this.host,
    required this.store,
    required this.platform,
  });

  final EngineHost host;
  final HostStore store;

  /// The platform family the engine lifecycle follows.
  final Platform platform;

  @override
  State<HoleBridgeApp> createState() => _HoleBridgeAppState();
}

class _HoleBridgeAppState extends State<HoleBridgeApp> {
  static const _vpn = _VpnNotReady();

  late final AppController _controller = AppController(widget.host, widget.store);
  late final EngineLifecycle _lifecycle = EngineLifecycle(widget.host, _vpn, widget.platform);

  /// The home screen follows this. A ValueNotifier, not a rebuilt MaterialApp.home, because the navigator
  /// keeps the first home it built.
  final ValueNotifier<_Engine> _engine = ValueNotifier(_Engine.starting);

  /// The navigator of the app, so a screen's Settings button can push the settings route.
  final GlobalKey<NavigatorState> _navigator = GlobalKey<NavigatorState>();

  /// Whether the lifecycle observer and the controller listener are added, so teardown removes them only
  /// when they were added.
  bool _observing = false;

  /// Whether the engine has started once. A retry restarts a started engine through the controller, and
  /// starts a host that never started.
  bool _started = false;

  @override
  void initState() {
    super.initState();
    unawaited(_start());
  }

  /// Starts the engine. The observer and the listener are added only after the start succeeds, so a
  /// failed start has no lifecycle calls waiting to reach the engine.
  Future<void> _start() async {
    _engine.value = _Engine.starting;
    try {
      await _controller.start();
    } catch (_) {
      // The error text is not shown or kept: the failed screen shows the code only.
      if (mounted) _engine.value = _Engine.failed;
      return;
    }
    if (!mounted) return;
    _started = true;
    if (!_observing) {
      _controller.addListener(_onController);
      WidgetsBinding.instance.addObserver(_lifecycle);
      _observing = true;
    }
    _engine.value = _Engine.running;
  }

  /// Retry from the engine-down screen. After a start, it restarts the engine through the controller: a
  /// restart that fails sets HB-ENGINE-DOWN again, and a restart that works clears it. Before a start, it
  /// starts again (see _EngineDownScreen).
  Future<void> _retry() async {
    if (_started) {
      await _controller.restartEngine();
    } else {
      await _start();
    }
  }

  /// Follows the controller's HB-ENGINE-DOWN: a restart after a crash that fails shows the engine-down
  /// screen, and a later restart that works shows the home screen again.
  void _onController() {
    if (!mounted) return;
    final down = _controller.lastErrorCode == _engineDownCode;
    if (down && _engine.value == _Engine.running) {
      _engine.value = _Engine.failed;
    } else if (!down && _engine.value == _Engine.failed) {
      _engine.value = _Engine.running;
    }
  }

  @override
  void dispose() {
    if (_observing) {
      _controller.removeListener(_onController);
      WidgetsBinding.instance.removeObserver(_lifecycle);
    }
    _observing = false;
    _controller.dispose();
    _engine.dispose();
    super.dispose();
  }

  /// Pushes the settings route. The Settings buttons of the home and host screens call it.
  void _openSettings() => unawaited(_navigator.currentState?.pushNamed<void>('/settings'));

  Widget _hostScreen(String hostId) => HostScreen(
    controller: _controller,
    store: widget.store,
    hostId: hostId,
    vpnMode: _vpn.enabled,
    onOpenSettings: _openSettings,
  );

  /// The first screen: the spinner until the engine runs, then the home screen. While the engine is down
  /// the navigator sits under the engine-down screen (see _overNavigator), so this shows the spinner there.
  Widget _home() => ValueListenableBuilder<_Engine>(
    valueListenable: _engine,
    builder: (context, engine, _) => switch (engine) {
      _Engine.starting || _Engine.failed => const Scaffold(
        body: Center(child: CircularProgressIndicator()),
      ),
      _Engine.running => HomeScreen(
        controller: _controller,
        store: widget.store,
        hostScreenBuilder: (context, hostId) => _hostScreen(hostId),
        onOpenSettings: _openSettings,
      ),
    },
  );

  /// Puts the engine-down screen above the navigator while the engine is down, so it covers a pushed route
  /// too. The navigator stays mounted underneath, so its routes keep their state. While covered it is
  /// excluded from focus, so the remote's focus goes to Retry and not to a route beneath, and from
  /// semantics, so a screen reader cannot reach a control beneath the cover.
  Widget _overNavigator(BuildContext context, Widget? child) => ValueListenableBuilder<_Engine>(
    valueListenable: _engine,
    builder: (context, engine, _) {
      final down = engine == _Engine.failed;
      return Stack(
        fit: StackFit.expand,
        children: [
          ExcludeSemantics(
            excluding: down,
            child: ExcludeFocus(
              excluding: down,
              child: TvFocusScope(child: child ?? const SizedBox.shrink()),
            ),
          ),
          if (down) _EngineDownScreen(onRetry: () => unawaited(_retry())),
        ],
      );
    },
  );

  Route<void> _route(RouteSettings settings) => MaterialPageRoute<void>(
    settings: settings,
    builder: (context) => switch (settings.name) {
      '/settings' => SettingsScreen(
        controller: _controller,
        store: widget.store,
        vpn: _vpn,
        onOpenDiagnostics: () => Navigator.of(context).pushNamed('/diagnostics'),
      ),
      '/add-host' => AddHostScreen(
        controller: _controller,
        store: widget.store,
        hostScreenBuilder: (context, hostId) => _hostScreen(hostId),
        onOpenSettings: _openSettings,
      ),
      '/diagnostics' => _DiagnosticsPage(controller: _controller, store: widget.store),
      '/host' => _hostScreen(settings.arguments! as String),
      _ => _home(),
    },
  );

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'HoleBridge',
      navigatorKey: _navigator,
      home: _home(),
      onGenerateRoute: _route,
      builder: _overNavigator,
    );
  }
}

/// Shown above the navigator (see _HoleBridgeAppState._overNavigator) when the engine did not start, or a
/// restart after a crash failed: its code and a retry. Before any start the retry calls AppController.start,
/// because restartEngine needs a client, and a start that failed before it built one has none. After a start
/// the retry calls AppController.restartEngine, which also covers a later failure.
class _EngineDownScreen extends StatelessWidget {
  const _EngineDownScreen({required this.onRetry});

  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) => Scaffold(
    body: SafeArea(
      child: Padding(
        padding: const EdgeInsets.all(16),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: [
            const Expanded(child: ErrorView(_engineDownCode, '')),
            const SizedBox(height: 16),
            FilledButton(autofocus: true, onPressed: onRetry, child: const Text('Retry')),
          ],
        ),
      ),
    ),
  );
}

/// The diagnostics screen with the route and NAT view of the first stored host, from
/// AppController.status. With no host stored, or when the engine does not answer, the screen shows no
/// route and no NAT view, as it does with no session.
class _DiagnosticsPage extends StatefulWidget {
  const _DiagnosticsPage({required this.controller, required this.store});

  final AppController controller;
  final HostStore store;

  @override
  State<_DiagnosticsPage> createState() => _DiagnosticsPageState();
}

class _DiagnosticsPageState extends State<_DiagnosticsPage> {
  late final Future<HostStatus?> _status = _load();

  Future<HostStatus?> _load() async {
    final hosts = await widget.store.hosts();
    if (hosts.isEmpty) return null;
    try {
      return await widget.controller.status(hosts.first.id);
    } on StateError {
      return null;
    } on TimeoutException {
      return null;
    } on IpcDesync {
      return null;
    }
  }

  @override
  Widget build(BuildContext context) => FutureBuilder<HostStatus?>(
    future: _status,
    builder: (context, snapshot) {
      if (snapshot.connectionState != ConnectionState.done) {
        return Scaffold(
          appBar: AppBar(title: const Text('Diagnostics')),
          body: const Center(child: CircularProgressIndicator()),
        );
      }
      final status = snapshot.data;
      return DiagnosticsScreen(
        controller: widget.controller,
        store: widget.store,
        route: status?.route ?? '',
        nat: status?.nat,
        appVersion: _appVersion,
        engineVersion: _engineVersion,
        protocolVersion: _protocolVersion,
      );
    },
  );
}
