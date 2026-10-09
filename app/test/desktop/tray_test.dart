// The desktop shell: close to tray, the tray menu and Start at login (docs/architecture.md#sessions-and-
// reconnects, the Desktop row). The window, the tray and the launch-at-login plugin are faked here.
import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/desktop/tray.dart';

import '../helpers/fake_engine_host.dart';

/// A window that records what the shell asks of it. [closeRequested] plays the user's close button.
class FakeWindow implements DesktopWindow {
  final List<String> calls = [];

  final StreamController<void> _closes = StreamController<void>.broadcast();

  @override
  Future<void> hide() async {
    calls.add('hide');
  }

  @override
  Future<void> show() async {
    calls.add('show');
  }

  @override
  Future<void> destroy() async {
    calls.add('destroy');
  }

  @override
  Stream<void> get closeRequests => _closes.stream;

  void closeRequested() => _closes.add(null);
}

/// A tray icon that records the menu it is given. [pick] plays the user picking a menu item by label.
class FakeTrayIcon implements TrayIcon {
  bool shown = false;

  List<TrayMenuItem> menu = const [];

  final StreamController<String> _picks = StreamController<String>.broadcast();

  @override
  Future<void> show() async {
    shown = true;
  }

  @override
  Future<void> setMenu(List<TrayMenuItem> items) async {
    menu = List.of(items);
  }

  @override
  Stream<String> get selections => _picks.stream;

  /// The labels of the menu, in order.
  List<String> get labels => [for (final item in menu) item.label];

  /// Emits the id of the menu item labelled [label], as a pick does.
  void pick(String label) => _picks.add(menu.singleWhere((item) => item.label == label).id);
}

/// Start at login as the fake plugin holds it. Its calls are recorded in order.
class FakeLaunchAtStartup implements LaunchAtStartup {
  FakeLaunchAtStartup({this.enabled = false});

  bool enabled;

  final List<String> calls = [];

  @override
  Future<bool> isEnabled() async => enabled;

  @override
  Future<void> enable() async {
    calls.add('enable');
    enabled = true;
  }

  @override
  Future<void> disable() async {
    calls.add('disable');
    enabled = false;
  }
}

/// The shell's listeners and host calls run asynchronously, so a test lets them finish before it looks.
Future<void> settle() => Future<void>.delayed(Duration.zero);

void main() {
  late FakeEngineHost engine;
  late FakeWindow window;
  late FakeTrayIcon tray;
  late FakeLaunchAtStartup launch;
  late DesktopShell shell;

  setUp(() async {
    engine = FakeEngineHost();
    await engine.start();
    window = FakeWindow();
    tray = FakeTrayIcon();
    launch = FakeLaunchAtStartup();
    shell = DesktopShell(window: window, tray: tray, launchAtStartup: launch, engine: engine);
  });

  test('closing the window hides it and the engine stays up', () async {
    await shell.start();

    window.closeRequested();
    await settle();

    expect(window.calls, ['hide'], reason: 'the close button hides the window, it does not end the app');
    expect(engine.calls, ['start'], reason: 'the engine keeps running in the tray, so it must not stop');
  });

  test('the tray menu lists hosts with their route, and Quit stops the engine', () async {
    await shell.start();
    await shell.updateHosts(const [
      TrayHost(id: 'h1', name: 'Home NAS', route: 'relay'),
      TrayHost(id: 'h2', name: 'Office', route: 'lan'),
    ]);
    await settle();

    expect(tray.shown, isTrue, reason: 'the tray icon is shown once the shell starts');
    expect(tray.labels, contains('Open HoleBridge'));
    expect(
      tray.labels.any((label) => label.contains('Home NAS') && label.contains('relay')),
      isTrue,
      reason: 'each host line carries its name and its route',
    );
    expect(
      tray.labels.any((label) => label.contains('Office') && label.contains('lan')),
      isTrue,
      reason: 'each host line carries its name and its route',
    );
    expect(tray.labels, contains('Quit'));

    tray.pick('Quit');
    await settle();

    expect(engine.calls, ['start', 'stop'], reason: 'Quit stops the engine');
    expect(window.calls, contains('destroy'), reason: 'Quit ends the app, not only the engine');
  });

  test('Start at login calls the launch-at-startup interface on and off', () async {
    await shell.start();

    expect(await shell.startAtLogin(), isFalse, reason: 'Start at login is off by default');

    await shell.setStartAtLogin(true);
    expect(launch.calls, ['enable'], reason: 'turning it on enables the launch-at-startup entry');
    expect(await shell.startAtLogin(), isTrue);

    await shell.setStartAtLogin(false);
    expect(launch.calls, ['enable', 'disable'], reason: 'turning it off removes the entry');
    expect(await shell.startAtLogin(), isFalse);
  });

  test('Open HoleBridge shows the window after a close hid it', () async {
    await shell.start();
    await shell.updateHosts(const []);

    window.closeRequested();
    await settle();
    tray.pick('Open HoleBridge');
    await settle();

    expect(window.calls, ['hide', 'show'], reason: 'Open HoleBridge brings the hidden window back');
    expect(engine.calls, ['start'], reason: 'opening the window does not touch the engine');
  });
}
