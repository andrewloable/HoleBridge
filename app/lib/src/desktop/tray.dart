// The desktop shell: a tray icon that lists the hosts and their routes, closing the window to the tray
// while the engine keeps running, and Start at login (docs/architecture.md#sessions-and-reconnects,
// docs/architecture.md#platforms). The plugins sit behind the small interfaces below, so the tests fake
// them.
import '../engine/engine_host.dart';

/// The app window. The IMPL wraps the window manager plugin, which is not a dependency yet.
abstract interface class DesktopWindow {
  /// Hides the window. The app keeps running.
  Future<void> hide();

  /// Shows the hidden window and brings it to the front. The menu's Open HoleBridge calls it.
  Future<void> show();

  /// Closes the window and ends the app.
  Future<void> destroy();

  /// Emits each time the user asks to close the window. The shell answers each one with a hide.
  Stream<void> get closeRequests;
}

/// The tray icon: the menu bar on macOS, the system tray on Windows, the panel on Linux. The IMPL wraps
/// tray_manager, which is not a dependency yet.
abstract interface class TrayIcon {
  /// Shows the icon.
  Future<void> show();

  /// Replaces the menu the icon shows.
  Future<void> setMenu(List<TrayMenuItem> items);

  /// Emits the id of each menu item the user picks.
  Stream<String> get selections;
}

/// One entry of the tray menu.
class TrayMenuItem {
  const TrayMenuItem({required this.id, required this.label});

  /// What a pick emits, so the shell can tell the items apart.
  final String id;

  /// The text the menu shows.
  final String label;
}

/// Start at login, through the platform's launch-at-login mechanism. The IMPL wraps launch_at_startup,
/// which is not a dependency yet.
abstract interface class LaunchAtStartup {
  Future<bool> isEnabled();
  Future<void> enable();
  Future<void> disable();
}

/// One host as the tray menu lists it.
class TrayHost {
  const TrayHost({required this.id, required this.name, required this.route});

  final String id;

  /// The name the user gave the host.
  final String name;

  /// lan, direct, relay, looking or unreachable, or empty when no session is up and no search runs.
  final String route;
}

/// Wires the window, the tray and Start at login to the engine. Closing the window hides it and the
/// engine keeps running. Quit stops the engine and ends the app.
class DesktopShell {
  DesktopShell({
    required this.window,
    required this.tray,
    required this.launchAtStartup,
    required this.engine,
  });

  final DesktopWindow window;
  final TrayIcon tray;
  final LaunchAtStartup launchAtStartup;
  final EngineHost engine;

  /// Shows the tray icon and starts handling close requests and menu picks. Call once.
  Future<void> start() {
    throw UnimplementedError();
  }

  /// Replaces the hosts the menu lists, with their routes. The app calls it when the controller changes.
  Future<void> updateHosts(List<TrayHost> hosts) {
    throw UnimplementedError();
  }

  /// Whether Start at login is on.
  Future<bool> startAtLogin() {
    throw UnimplementedError();
  }

  /// Turns Start at login on or off.
  Future<void> setStartAtLogin(bool enabled) {
    throw UnimplementedError();
  }

  /// Stops the engine and ends the app. The menu's Quit calls it.
  Future<void> quit() {
    throw UnimplementedError();
  }
}
