// The engine lifecycle per platform and VPN mode (docs/architecture.md#sessions-and-reconnects,
// docs/architecture.md#platforms, docs/architecture.md#vpn-mode-android-and-ios).
//
// The lifecycle only pauses and resumes the engine. Every test checks the exact host calls, so a
// lifecycle that also stops or starts the engine fails. The controller's registrations and relay flag
// stay valid only while the same worklet keeps running.
import 'dart:async';

import 'package:flutter/widgets.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/engine/lifecycle.dart';

import '../helpers/fake_engine_host.dart';

/// VPN mode as a test sets it.
class FakeVpnState implements VpnState {
  const FakeVpnState({required this.enabled});

  @override
  final bool enabled;
}

/// A host whose suspend records its call and then runs [onSuspend], so a test can hold the call open
/// or make it fail. Resume is the FakeEngineHost's.
class _SuspendHost extends FakeEngineHost {
  _SuspendHost(this.onSuspend);

  final Future<void> Function() onSuspend;

  @override
  Future<void> suspend() {
    calls.add('suspend');
    return onSuspend();
  }
}

/// The lifecycle's host calls run asynchronously, so a test lets them finish before it looks.
Future<void> settle() => Future<void>.delayed(Duration.zero);

void main() {
  setUpAll(() => TestWidgetsFlutterBinding.ensureInitialized());

  test('desktop: paused and hidden do not suspend the engine, which keeps running in the tray', () async {
    final host = FakeEngineHost();
    await host.start();
    final lifecycle = EngineLifecycle(host, const FakeVpnState(enabled: false), Platform.desktop);

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.paused);
    await settle();
    expect(host.calls, ['start'], reason: 'the window is closed to the tray, so the engine must stay up');

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.hidden);
    await settle();
    expect(host.calls, ['start'], reason: 'a hidden window is the tray case, so no suspend');

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.resumed);
    await settle();
    expect(host.calls, ['start'], reason: 'the engine was never suspended, so there is nothing to resume');
  });

  test('android with VPN mode on: paused does not suspend, because the VpnService keeps the engine running',
      () async {
    final host = FakeEngineHost();
    await host.start();
    final lifecycle = EngineLifecycle(host, const FakeVpnState(enabled: true), Platform.android);

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.paused);
    await settle();
    expect(host.calls, ['start'], reason: 'VPN mode on keeps the engine running in the background');

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.resumed);
    await settle();
    expect(host.calls, ['start'], reason: 'the engine was never suspended, so there is nothing to resume');
  });

  test('android with VPN mode off: paused suspends the engine and resumed resumes it', () async {
    final host = FakeEngineHost();
    await host.start();
    final lifecycle = EngineLifecycle(host, const FakeVpnState(enabled: false), Platform.android);

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.paused);
    await settle();
    expect(host.calls, ['start', 'suspend'], reason: 'listeners exist only while the app is open');

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.resumed);
    await settle();
    expect(
      host.calls,
      ['start', 'suspend', 'resume'],
      reason: 'the same worklet resumes, so no stop or start, and no controller reset is needed',
    );
  });

  test('ios: paused suspends the engine, and resumed resumes it and sends nothing until a session is asked for',
      () async {
    final host = FakeEngineHost();
    await host.start();
    final lifecycle = EngineLifecycle(host, const FakeVpnState(enabled: false), Platform.ios);

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.paused);
    await settle();
    expect(host.calls, ['start', 'suspend'], reason: 'iOS suspends a backgrounded app');

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.resumed);
    await settle();
    expect(host.calls, ['start', 'suspend', 'resume'], reason: 'the same worklet resumes');
    expect(
      host.sent,
      isEmpty,
      reason: 'the lifecycle does not reconnect: a session opens on demand, when a tile is tapped',
    );
  });

  test('ios: a resume sent while the suspend is in flight waits for it, so the engine ends resumed', () async {
    final suspending = Completer<void>();
    final host = _SuspendHost(() => suspending.future);
    await host.start();
    final lifecycle = EngineLifecycle(host, const FakeVpnState(enabled: false), Platform.ios);

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.paused);
    lifecycle.didChangeAppLifecycleState(AppLifecycleState.resumed);
    await settle();
    expect(
      host.calls,
      ['start', 'suspend'],
      reason: 'the resume must not reach the worklet until the suspend has returned',
    );

    suspending.complete();
    await settle();
    expect(
      host.calls,
      ['start', 'suspend', 'resume'],
      reason: 'the worklet sets its state to suspended only after the suspend returns, so the resume follows it',
    );
  });

  test('ios: a suspend that throws synchronously is reported through FlutterError, and the resume still runs',
      () async {
    final reported = <FlutterErrorDetails>[];
    final old = FlutterError.onError;
    FlutterError.onError = reported.add;
    addTearDown(() {
      FlutterError.onError = old;
    });
    final host = _SuspendHost(() => throw StateError('the engine is not started'));
    await host.start();
    final lifecycle = EngineLifecycle(host, const FakeVpnState(enabled: false), Platform.ios);

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.paused);
    await settle();
    expect(reported, hasLength(1), reason: 'the failure must be reported, not thrown out of the callback');

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.resumed);
    await settle();
    expect(
      host.calls,
      ['start', 'suspend', 'resume'],
      reason: 'a failed suspend must not stop the resume',
    );
  });

  test('ios: a suspend that fails asynchronously is reported through FlutterError, not left unhandled', () async {
    final reported = <FlutterErrorDetails>[];
    final old = FlutterError.onError;
    FlutterError.onError = reported.add;
    addTearDown(() {
      FlutterError.onError = old;
    });
    final host = _SuspendHost(() => Future<void>.error(StateError('platform')));
    await host.start();
    final lifecycle = EngineLifecycle(host, const FakeVpnState(enabled: false), Platform.ios);

    lifecycle.didChangeAppLifecycleState(AppLifecycleState.paused);
    await settle();
    expect(
      reported,
      hasLength(1),
      reason: 'the platform failure must be reported; an unhandled async error would fail this test too',
    );
  });
}
