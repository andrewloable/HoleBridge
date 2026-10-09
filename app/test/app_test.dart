// The app root (lib/src/app.dart, run by lib/main.dart). It starts the engine once, registers the engine
// lifecycle after the start, shows the first screen, and on teardown removes the lifecycle and disposes
// the controller. The engine is a FakeEngineHost, so every host call is recorded in order.
import 'dart:typed_data';

import 'package:flutter/material.dart';
import 'package:flutter/semantics.dart';
import 'package:flutter/services.dart' show LogicalKeyboardKey;
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/app.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';
import 'package:holebridge/src/engine/lifecycle.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';
import 'package:holebridge/src/ui/add_host_screen.dart';
import 'package:holebridge/src/ui/diagnostics_screen.dart';
import 'package:holebridge/src/ui/host_screen.dart';
import 'package:holebridge/src/ui/settings_screen.dart';

import 'helpers/fake_engine_host.dart';

// Test values only (docs/security.md): a 9-symbol key and a 32-byte application key.
const _keyA = '7KQM4X9TR';
final _appKey = Uint8List.fromList(List<int>.generate(32, (i) => i));

/// An in-memory SecureBackend, as the one in test/app_controller_test.dart is.
class _MemoryBackend implements SecureBackend {
  final Map<String, String> entries = {};

  @override
  Future<String?> read(String k) async {
    return entries[k];
  }

  @override
  Future<void> write(String k, String v) async {
    entries[k] = v;
  }

  @override
  Future<void> delete(String k) async {
    entries.remove(k);
  }
}

/// Lets the start, the store read and the lifecycle calls finish. It pumps a fixed number of frames
/// rather than pumpAndSettle, because a progress indicator on screen never settles.
Future<void> _settle(WidgetTester tester) async {
  for (var i = 0; i < 5; i++) {
    await tester.pump(const Duration(milliseconds: 20));
  }
}

/// The labels in the semantics tree, which is what a screen reader reaches. The tree is walked here rather
/// than searched with bySemanticsLabel: that finder reads each render object's cached semantics node, and
/// the cache of a subtree that ExcludeSemantics has dropped is left behind, so the finder still matches it.
List<String> _reachableLabels(WidgetTester tester) {
  final labels = <String>[];
  void visit(SemanticsNode node) {
    if (node.label.isNotEmpty) labels.add(node.label);
    node.visitChildren((child) {
      visit(child);
      return true;
    });
  }

  final root = tester.binding.renderViews.firstOrNull?.owner?.semanticsOwner?.rootSemanticsNode;
  if (root != null) visit(root);
  return labels;
}

Future<void> _pumpApp(
  WidgetTester tester,
  FakeEngineHost host, {
  required Platform platform,
}) {
  return tester.pumpWidget(
    HoleBridgeApp(host: host, store: HostStore(_MemoryBackend()), platform: platform),
  );
}

void main() {
  testWidgets('starts the engine host once at app start', (tester) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.android);
    await _settle(tester);

    expect(host.calls, ['start'], reason: 'the engine starts once, at app start');
  });

  testWidgets('shows the first-run screen (add a host) with an empty store', (tester) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.android);
    await _settle(tester);

    expect(
      find.byType(AddHostScreen),
      findsOneWidget,
      reason: 'a fresh install opens on the add-host screen',
    );
  });

  testWidgets('a paused event on Android with VPN off reaches the engine as a suspend', (
    tester,
  ) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.android);
    await _settle(tester);

    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
    await _settle(tester);

    expect(
      host.calls,
      ['start', 'suspend'],
      reason: 'Android with VPN off suspends the engine in the background',
    );
  });

  testWidgets('the lifecycle observer is removed on teardown', (tester) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.android);
    await _settle(tester);

    await tester.pumpWidget(const SizedBox());
    await _settle(tester);

    tester.binding.handleAppLifecycleStateChanged(AppLifecycleState.paused);
    await _settle(tester);

    expect(
      host.calls,
      ['start'],
      reason: 'after teardown a paused event must not reach the engine',
    );
  });

  testWidgets('a failed start shows the engine-down error, and its retry starts the engine again', (
    tester,
  ) async {
    final host = FakeEngineHost()..failNextStart = true;
    await _pumpApp(tester, host, platform: Platform.desktop);
    await _settle(tester);

    expect(find.text('HB-ENGINE-DOWN'), findsOneWidget);
    expect(find.byType(AddHostScreen), findsNothing);

    await tester.tap(find.text('Retry'));
    await _settle(tester);

    expect(host.calls, ['start', 'start'], reason: 'the retry starts the engine a second time');
    expect(find.text('HB-ENGINE-DOWN'), findsNothing);
    expect(find.byType(AddHostScreen), findsOneWidget);
  });

  testWidgets('a failed restart after a crash shows the engine-down error, and its retry restarts the engine', (
    tester,
  ) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.desktop);
    await _settle(tester);
    expect(find.byType(AddHostScreen), findsOneWidget);

    host.failNextStart = true;
    host.crash();
    await _settle(tester);

    expect(host.calls, ['start', 'stop', 'start']);
    expect(find.text('HB-ENGINE-DOWN'), findsOneWidget);

    await tester.tap(find.text('Retry'));
    await _settle(tester);

    expect(host.calls, ['start', 'stop', 'start', 'stop', 'start']);
    expect(find.text('HB-ENGINE-DOWN'), findsNothing);
    expect(find.byType(AddHostScreen), findsOneWidget);
  });

  testWidgets('the add-host screen links to Settings, and the link opens the settings screen', (
    tester,
  ) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.desktop);
    await _settle(tester);
    expect(find.byType(SettingsScreen), findsNothing);

    await tester.tap(find.byTooltip('Settings'));
    await _settle(tester);

    expect(find.byType(SettingsScreen), findsOneWidget);
  });

  testWidgets('the host screen links to Settings, and the link opens the settings screen', (
    tester,
  ) async {
    final store = HostStore(_MemoryBackend());
    await tester.runAsync(() => store.addHost(name: 'living-room', key: _keyA, appKey: _appKey));
    final host = FakeEngineHost();
    // The engine refuses the connect the host screen sends at once, as the host screen's tests do, so no
    // request is left waiting on a timer.
    host.onRequest = (request) => host.deliver(
      encode(IpcReply(id: request.id, ok: false, code: 'HB-VERSION-MISMATCH')),
    );
    await tester.pumpWidget(HoleBridgeApp(host: host, store: store, platform: Platform.desktop));
    await _settle(tester);
    expect(find.byType(HostScreen), findsOneWidget);

    await tester.tap(find.byTooltip('Settings'));
    await _settle(tester);

    expect(find.byType(SettingsScreen), findsOneWidget);
  });

  testWidgets('Settings links to Diagnostics, which opens the diagnostics screen', (tester) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.desktop);
    await _settle(tester);
    await tester.tap(find.byTooltip('Settings'));
    await _settle(tester);

    await tester.tap(find.text('Diagnostics'));
    await _settle(tester);

    expect(find.byType(DiagnosticsScreen), findsOneWidget);
  });

  testWidgets(
    'a failed restart shows the engine-down error with Retry above Settings, and Retry clears it',
    (tester) async {
      final semantics = tester.ensureSemantics();
      final host = FakeEngineHost();
      await _pumpApp(tester, host, platform: Platform.desktop);
      await _settle(tester);
      await tester.tap(find.byTooltip('Settings'));
      await _settle(tester);
      expect(find.byType(SettingsScreen), findsOneWidget);

      host.failNextStart = true;
      host.crash();
      await _settle(tester);

      expect(host.calls, ['start', 'stop', 'start']);
      expect(find.text('HB-ENGINE-DOWN'), findsOneWidget);
      expect(find.widgetWithText(FilledButton, 'Retry'), findsOneWidget);
      expect(
        _reachableLabels(tester).where((label) => label.contains('Diagnostics')),
        isEmpty,
        reason: 'the screen under the engine-down screen is not reachable by a screen reader',
      );
      expect(find.bySemanticsLabel('Retry'), findsOneWidget);
      expect(
        Focus.of(tester.element(find.text('Retry'))).hasPrimaryFocus,
        isTrue,
        reason: 'the remote lands on Retry, not on a control of the settings screen under it',
      );

      await tester.tap(find.text('Diagnostics'), warnIfMissed: false);
      await _settle(tester);
      expect(
        find.byType(DiagnosticsScreen),
        findsNothing,
        reason: 'the engine-down screen covers the settings route, so a tap under it does nothing',
      );

      await tester.tap(find.widgetWithText(FilledButton, 'Retry'));
      await _settle(tester);

      expect(host.calls, ['start', 'stop', 'start', 'stop', 'start']);
      expect(find.text('HB-ENGINE-DOWN'), findsNothing);
      expect(
        find.byType(SettingsScreen),
        findsOneWidget,
        reason: 'once the restart works the settings screen is back',
      );
      expect(
        _reachableLabels(tester).where((label) => label.contains('Diagnostics')),
        isNotEmpty,
        reason: 'the settings screen is back in the semantics tree once the engine runs',
      );
      semantics.dispose();
    },
  );

  testWidgets('arrow-down from the relay key field moves focus to Save, through the app TvFocusScope', (
    tester,
  ) async {
    final host = FakeEngineHost();
    await _pumpApp(tester, host, platform: Platform.desktop);
    await _settle(tester);
    await tester.tap(find.byTooltip('Settings'));
    await _settle(tester);
    expect(find.byType(SettingsScreen), findsOneWidget);

    final relayField = find.byKey(const Key('relay-key-field'));
    await tester.tap(relayField);
    await tester.pump();
    final fieldFocus = tester
        .widget<EditableText>(find.descendant(of: relayField, matching: find.byType(EditableText)))
        .focusNode;
    expect(fieldFocus.hasPrimaryFocus, isTrue, reason: 'the relay key field has focus to start with');

    await tester.sendKeyEvent(LogicalKeyboardKey.arrowDown);
    await tester.pump();

    expect(fieldFocus.hasPrimaryFocus, isFalse, reason: 'arrow-down leaves the single-line text field');
    expect(
      Focus.of(tester.element(find.text('Save'))).hasPrimaryFocus,
      isTrue,
      reason: 'arrow-down from the relay key field reaches Save, the control below it',
    );
  });
}
