// Widget tests for the host screen (docs/cli.md#the-app, docs/architecture.md#service-kinds). The screen is
// driven through a real AppController over a FakeEngineHost, as test/app_controller_test.dart drives the
// controller. The FakeEngineHost records the requests the controller sends and delivers the engine's frames,
// which is the fake client these tests need: the controller builds its own EngineClient on the host.

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/app_controller.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';
import 'package:holebridge/src/ui/host_screen.dart';
import 'package:holebridge/src/ui/service_tile.dart';

import '../helpers/fake_engine_host.dart';

/// An in-memory SecureBackend, as the one in test/app_controller_test.dart is.
class _MemoryBackend implements SecureBackend {
  final Map<String, String> _entries = {};

  @override
  Future<String?> read(String k) async => _entries[k];

  @override
  Future<void> write(String k, String v) async {
    _entries[k] = v;
  }

  @override
  Future<void> delete(String k) async {
    _entries.remove(k);
  }
}

// Test values only (docs/security.md): a 9-symbol key in the Crockford alphabet and a 32-byte application key.
const _hostName = 'living-room';
const _keyA = '7KQM4X9TR';
final _appKey = Uint8List.fromList(List<int>.generate(32, (i) => i));

/// One service of each kind, the unknown kind included, as the engine's services list carries them.
const _services = [
  ServiceEntry(name: 'web', kind: 1),
  ServiceEntry(name: 'jellyfin', kind: 2),
  ServiceEntry(name: 'ssh', kind: 3),
  ServiceEntry(name: 'dns', kind: 4),
  ServiceEntry(name: 'misc', kind: 0),
];

/// The local port bound for each service once a session is up.
const _ports = [
  PortBinding(service: 'web', port: 8443),
  PortBinding(service: 'jellyfin', port: 8096),
  PortBinding(service: 'ssh', port: 53817),
  PortBinding(service: 'dns', port: 5353),
  PortBinding(service: 'misc', port: 9100),
];

/// A responder that refuses every request with [code]. HB-VERSION-MISMATCH is the default refusal: the
/// engine keeps no host registered after it (docs/architecture.md, routes), so a later connect is sent again.
void Function(IpcRequest) _refuse(FakeEngineHost host, String code) {
  return (request) {
    host.deliver(encode(IpcReply(id: request.id, ok: false, code: code)));
  };
}

/// A responder like a reachable engine: close is ok, and connect is ok with the route lan, the services and
/// the ports bound. A port the connect asked for is the port bound.
void Function(IpcRequest) _reachable(FakeEngineHost host) {
  return (request) {
    if (request is ConnectRequest) {
      final asked = {for (final port in request.ports) port.service: port.port};
      host.deliver(
        encode(
          IpcReply(
            id: request.id,
            ok: true,
            route: 'lan',
            services: _services,
            ports: [
              for (final port in _ports)
                PortBinding(service: port.service, port: asked[port.service] ?? port.port),
            ],
          ),
        ),
      );
    } else {
      host.deliver(encode(IpcReply(id: request.id, ok: true)));
    }
  };
}

/// What one test drives: the fake engine, the store with one host, the controller started over the engine,
/// and the id of the host.
class _Harness {
  _Harness(this.host, this.store, this.controller, this.hostId);

  final FakeEngineHost host;
  final HostStore store;
  final AppController controller;
  final String hostId;
}

/// Starts a controller over a fake engine with one stored host, named [name]. The host's cached services are
/// [_services], so the screen has tiles before any session is up, with no local port bound.
Future<_Harness> _started({String name = _hostName}) async {
  final store = HostStore(_MemoryBackend());
  final added = await store.addHost(name: name, key: _keyA, appKey: _appKey);
  await store.saveServices(added.id, [for (final service in _services) service.name]);
  await store.saveServiceKinds(added.id, {
    for (final service in _services) service.name: service.kind,
  });
  final host = FakeEngineHost();
  addTearDown(host.close);
  host.onRequest = _refuse(host, 'HB-VERSION-MISMATCH');
  final controller = AppController(host, store);
  addTearDown(controller.dispose);
  await controller.start();
  return _Harness(host, store, controller, added.id);
}

/// The host screen for [h], in a MaterialApp.
Widget _screen(_Harness h, {bool vpnMode = false}) {
  return MaterialApp(
    home: HostScreen(controller: h.controller, store: h.store, hostId: h.hostId, vpnMode: vpnMode),
  );
}

/// Pumps the screen for [h] and fails at once if it does not build, so a build error is the failure that
/// is reported, not the finders that run after it.
Future<void> _pumpScreen(WidgetTester tester, _Harness h, {bool vpnMode = false}) async {
  await tester.pumpWidget(_screen(h, vpnMode: vpnMode));
  expect(tester.takeException(), isNull, reason: 'the host screen builds');
}

/// A surface tall enough for every tile, so each one is built and can be tapped.
void _tallSurface(WidgetTester tester) {
  tester.view.physicalSize = const Size(800, 2000);
  tester.view.devicePixelRatio = 1;
  addTearDown(tester.view.resetPhysicalSize);
  addTearDown(tester.view.resetDevicePixelRatio);
}

/// The tile of the service [name]: the ServiceTile that holds the name's text.
Finder _tile(String name) {
  return find.ancestor(of: find.text(name), matching: find.byType(ServiceTile));
}

/// What [inside] finds within the tile of the service [name].
Finder _inTile(String name, Finder inside) {
  return find.descendant(of: _tile(name), matching: inside);
}

void main() {
  group('the host screen', () {
    testWidgets('a lan route shows the LAN badge, and a looking route shows Looking for host...', (
      tester,
    ) async {
      final h = await _started();
      await _pumpScreen(tester, h);

      h.host.deliver(encode(const RouteEvent(host: _hostName, route: 'lan')));
      await tester.pumpAndSettle();
      expect(find.text('LAN'), findsOneWidget);
      expect(find.text('Looking for host...'), findsNothing);

      h.host.deliver(encode(const RouteEvent(host: _hostName, route: 'looking')));
      await tester.pumpAndSettle();
      expect(find.text('Looking for host...'), findsOneWidget);
      expect(find.text('LAN'), findsNothing);
    });

    testWidgets('a direct route shows the Direct badge and a relay route shows the Relay badge', (
      tester,
    ) async {
      final h = await _started();
      await _pumpScreen(tester, h);

      h.host.deliver(encode(const RouteEvent(host: _hostName, route: 'direct')));
      await tester.pumpAndSettle();
      expect(find.text('Direct'), findsOneWidget);

      h.host.deliver(encode(const RouteEvent(host: _hostName, route: 'relay')));
      await tester.pumpAndSettle();
      expect(find.text('Relay'), findsOneWidget);
      expect(find.text('Direct'), findsNothing);
    });

    testWidgets("an unreachable host shows Can't reach host and the error code", (tester) async {
      final h = await _started();
      // The connect is refused with the lookup's timeout, so the controller keeps the code for the host.
      h.host.onRequest = _refuse(h.host, 'HB-LOOKUP-TIMEOUT');
      await h.controller.connect(h.hostId);
      await _pumpScreen(tester, h);

      h.host.deliver(encode(const RouteEvent(host: _hostName, route: 'unreachable')));
      await tester.pumpAndSettle();

      expect(find.textContaining("Can't reach host"), findsWidgets);
      expect(find.textContaining('HB-LOOKUP-TIMEOUT'), findsWidgets);
    });

    testWidgets(
      'tiles follow the kind: https and http offer Open, tcp and udp the address, unknown Try opening',
      (tester) async {
        _tallSurface(tester);
        final h = await _started();
        await _pumpScreen(tester, h);

        h.host.deliver(encode(ServicesEvent(host: _hostName, list: _services, ports: _ports)));
        await tester.pumpAndSettle();

        for (final name in ['web', 'jellyfin']) {
          expect(_inTile(name, find.text('Open')), findsOneWidget, reason: '$name shows Open');
          expect(
            _inTile(name, find.text('Use the native app')),
            findsOneWidget,
            reason: '$name shows Use the native app',
          );
          expect(
            _inTile(name, find.text('Copy address')),
            findsNothing,
            reason: '$name has no Copy address',
          );
        }

        expect(_inTile('ssh', find.textContaining('127.0.0.1:53817')), findsOneWidget);
        expect(_inTile('ssh', find.text('Copy address')), findsOneWidget);
        expect(_inTile('ssh', find.text('Open')), findsNothing);

        expect(_inTile('dns', find.textContaining('127.0.0.1:5353')), findsOneWidget);
        expect(_inTile('dns', find.text('Copy address')), findsOneWidget);
        expect(_inTile('dns', find.text('Open')), findsNothing);

        expect(_inTile('misc', find.text('Try opening')), findsOneWidget);
        expect(_inTile('misc', find.text('Copy address')), findsOneWidget);
        expect(_inTile('misc', find.text('Open')), findsNothing);
      },
    );

    testWidgets('Copy address copies 127.0.0.1:<port>, or the service name with VPN mode on', (
      tester,
    ) async {
      _tallSurface(tester);
      final h = await _started();
      await _pumpScreen(tester, h);
      h.host.deliver(encode(ServicesEvent(host: _hostName, list: _services, ports: _ports)));
      await tester.pumpAndSettle();

      String? copied;
      tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (
        call,
      ) async {
        if (call.method == 'Clipboard.setData') {
          copied = (call.arguments as Map)['text'] as String?;
        }
        return null;
      });
      addTearDown(
        () => tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
          SystemChannels.platform,
          null,
        ),
      );

      await tester.tap(_inTile('ssh', find.text('Copy address')));
      await tester.pumpAndSettle();
      expect(copied, '127.0.0.1:53817');

      await _pumpScreen(tester, h, vpnMode: true);
      await tester.pumpAndSettle();
      await tester.tap(_inTile('ssh', find.text('Copy address')));
      await tester.pumpAndSettle();
      expect(copied, 'ssh.$_hostName.internal');
    });

    testWidgets('setting a port by hand saves it and sends a connect with the remembered port', (
      tester,
    ) async {
      _tallSurface(tester);
      final h = await _started();
      await _pumpScreen(tester, h);
      await tester.pumpAndSettle();
      final connectsBefore = h.host.requestsOf<ConnectRequest>().length;

      await tester.longPress(_inTile('ssh', find.text('ssh')));
      await tester.pumpAndSettle();
      await tester.enterText(find.byType(TextField), '2233');
      await tester.tap(find.text('Save'));
      await tester.pumpAndSettle();

      final ports = await h.store.ports(h.hostId);
      expect(ports['ssh'], 2233);

      final connects = h.host.requestsOf<ConnectRequest>().toList();
      expect(connects.length, greaterThan(connectsBefore));
      expect(
        connects.last.ports.any((port) => port.service == 'ssh' && port.port == 2233),
        isTrue,
        reason: 'the new connect carries the port set by hand',
      );
    });

    testWidgets(
      'setting a port by hand on a connected host closes it and connects again with the new port',
      (tester) async {
        _tallSurface(tester);
        final h = await _started();
        h.host.onRequest = _reachable(h.host);
        // The connect is answered ok, so the engine has the host registered, as it has once a session is up.
        await h.controller.connect(h.hostId);
        await _pumpScreen(tester, h);
        await tester.pumpAndSettle();
        final before = h.host.requests.length;

        await tester.longPress(_inTile('ssh', find.text('ssh')));
        await tester.pumpAndSettle();
        await tester.enterText(find.byType(TextField), '2233');
        await tester.tap(find.text('Save'));
        await tester.pumpAndSettle();

        expect((await h.store.ports(h.hostId))['ssh'], 2233);
        final sent = h.host.requests.skip(before).toList();
        final closeAt = sent.indexWhere((r) => r is CloseRequest && r.host == _hostName);
        final connectAt = sent.indexWhere((r) => r is ConnectRequest);
        expect(closeAt, isNonNegative, reason: 'the host is closed first');
        expect(connectAt, isNonNegative, reason: 'a connect is sent');
        expect(connectAt, greaterThan(closeAt), reason: 'the connect follows the close');
        final connect = sent[connectAt] as ConnectRequest;
        expect(
          connect.ports.any((port) => port.service == 'ssh' && port.port == 2233),
          isTrue,
          reason: 'the new connect carries the port set by hand',
        );
        expect(_inTile('ssh', find.textContaining('127.0.0.1:2233')), findsOneWidget);
      },
    );

    testWidgets(
      "a lookup that is retried shows Looking for host... and no longer Can't reach host",
      (tester) async {
        final h = await _started();
        h.host.onRequest = _refuse(h.host, 'HB-LOOKUP-TIMEOUT');
        await h.controller.connect(h.hostId);
        await _pumpScreen(tester, h);
        h.host.deliver(encode(const RouteEvent(host: _hostName, route: 'unreachable')));
        await tester.pumpAndSettle();
        expect(find.textContaining("Can't reach host"), findsWidgets);

        // The engine retries on its own. The controller keeps the code of the failed lookup through looking.
        h.host.deliver(encode(const RouteEvent(host: _hostName, route: 'looking')));
        await tester.pumpAndSettle();
        expect(h.controller.view(h.hostId).lastErrorCode, 'HB-LOOKUP-TIMEOUT');
        expect(find.text('Looking for host...'), findsOneWidget);
        expect(find.textContaining("Can't reach host"), findsNothing);
        expect(find.textContaining('HB-LOOKUP-TIMEOUT'), findsNothing);
      },
    );

    testWidgets('with VPN mode on, the copied name is the host name lowercased with dashes', (
      tester,
    ) async {
      _tallSurface(tester);
      final h = await _started(name: 'Living Room');
      await _pumpScreen(tester, h, vpnMode: true);
      h.host.deliver(encode(ServicesEvent(host: 'Living Room', list: _services, ports: _ports)));
      await tester.pumpAndSettle();

      String? copied;
      tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(SystemChannels.platform, (
        call,
      ) async {
        if (call.method == 'Clipboard.setData') {
          copied = (call.arguments as Map)['text'] as String?;
        }
        return null;
      });
      addTearDown(
        () => tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
          SystemChannels.platform,
          null,
        ),
      );

      await tester.tap(_inTile('ssh', find.text('Copy address')));
      await tester.pumpAndSettle();
      expect(copied, 'ssh.living-room.internal');
    });
  });
}
