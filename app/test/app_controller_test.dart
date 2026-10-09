import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/app_controller.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';
import 'package:holebridge/src/keys/normalize.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';

import 'helpers/fake_engine_host.dart';

/// An in-memory SecureBackend, as the one in test/store/host_store_test.dart is.
class _MemoryBackend implements SecureBackend {
  final Map<String, String> _entries = {};

  @override
  Future<String?> read(String k) async {
    return _entries[k];
  }

  @override
  Future<void> write(String k, String v) async {
    _entries[k] = v;
  }

  @override
  Future<void> delete(String k) async {
    _entries.remove(k);
  }
}

// Test values only (docs/security.md): application keys of 32 bytes, and 9-symbol keys in the
// Crockford alphabet. The typed key has lower case and dashes, and normalizes to the third value.
final _appKeyA = Uint8List.fromList(List<int>.generate(32, (i) => i));
final _appKeyB = Uint8List.fromList(List<int>.filled(32, 0xff));
const _keyA = '7KQM4X9TR';
const _keyB = 'HJKMNPQRS';
const _relayKey = 'ZXVTSRQPN';
const _typedKey = 'pqr-stv-wxy';
const _typedKeyNormalized = 'PQRSTVWXY';
// A relay key typed with lower case and dashes, and the form it normalizes to. It differs from _relayKey.
const _relayTyped = 'rst-vwx-yzq';
const _relayTypedNormalized = 'RSTVWXYZQ';

/// The base64 form of [bytes], to compare application keys in sets.
String _b64(Uint8List bytes) => base64Encode(bytes);

/// A fake engine that answers every request ok, as the engine does for a request that needs no
/// more than that.
FakeEngineHost _answeringHost() {
  final host = FakeEngineHost();
  host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
  return host;
}

/// The wire service kinds with the names the view shows for them. Kind 99 is out of the wire's range.
const _kindCases = {0: 'unknown', 1: 'https', 2: 'http', 3: 'tcp', 4: 'udp', 99: 'unknown'};

/// Starts a controller over [host] and [store]. The controller is disposed when the test ends.
Future<AppController> _started(FakeEngineHost host, HostStore store) async {
  final controller = AppController(host, store);
  addTearDown(controller.dispose);
  await controller.start();
  return controller;
}

void main() {
  group('AppController', () {
    test('start() sends a relay request when a relay key is stored', () async {
      final store = HostStore(_MemoryBackend());
      // One application key is held, so the relay request carries it.
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);

      await controller.start();

      final relay = host.requestsOf<RelayRequest>().single;
      expect(relay.key, _relayKey);
      expect(relay.appKey, _appKeyA);
    });

    test('connect() sends connect with the host key, app key, stored LAN addresses and remembered ports', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveLan(added.id, ['192.168.1.20']);
      await store.savePort(added.id, 'ssh', 2222);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();

      await controller.connect(added.id);

      final connect = host.requestsOf<ConnectRequest>().single;
      expect(connect.host, 'Home', reason: 'the host field carries the saved name');
      expect(connect.key, _keyA);
      expect(connect.appKey, _appKeyA);
      expect(connect.bind, '127.0.0.1', reason: 'loopback unless Share with my network is on');
      expect(connect.lan.addresses, ['192.168.1.20']);
      expect(connect.ports.single.service, 'ssh');
      expect(connect.ports.single.port, 2222);
    });

    test('a services event updates the view and saves the cache and ports', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();
      var notified = false;
      controller.addListener(() => notified = true);

      host.deliver(
        encode(
          ServicesEvent(
            host: 'Home',
            list: const [
              ServiceEntry(name: 'ssh', kind: 3),
              ServiceEntry(name: 'web', kind: 2),
            ],
            ports: const [
              PortBinding(service: 'ssh', port: 2222),
              PortBinding(service: 'web', port: 8080),
            ],
          ),
        ),
      );
      await pumpEventQueue();
      expect(notified, isTrue, reason: 'listeners are told the view changed');

      final view = controller.view(added.id);
      expect([for (final service in view.services) service.name], ['ssh', 'web']);
      expect(view.services.firstWhere((s) => s.name == 'ssh').port, 2222);
      expect(view.services.firstWhere((s) => s.name == 'web').port, 8080);
      expect(await store.servicesCache(added.id), ['ssh', 'web']);
      expect(await store.ports(added.id), {'ssh': 2222, 'web': 8080});
    });

    test("a route event 'looking' then 'direct' updates the badge state in order", () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();
      final seen = <String>[];
      controller.addListener(() {
        final route = controller.view(added.id).route;
        if (seen.isEmpty || seen.last != route) seen.add(route);
      });

      host.deliver(encode(const RouteEvent(host: 'Home', route: 'looking')));
      await pumpEventQueue();
      expect(controller.view(added.id).route, 'looking');

      host.deliver(encode(const RouteEvent(host: 'Home', route: 'direct')));
      await pumpEventQueue();
      expect(controller.view(added.id).route, 'direct');
      expect(seen, ['looking', 'direct'], reason: 'listeners are told of each change, in order');
    });

    test('a reject event sets the last error code on the host view', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();
      var notified = false;
      controller.addListener(() => notified = true);

      host.deliver(
        encode(
          const RejectEvent(
            host: 'Home',
            service: 'ssh',
            code: 'HB-TARGET-REFUSED',
            reason: 'the target refused the connection',
          ),
        ),
      );
      await pumpEventQueue();
      expect(notified, isTrue, reason: 'listeners are told the view changed');

      expect(controller.view(added.id).lastErrorCode, 'HB-TARGET-REFUSED');
    });

    test('addTypedKey with two held app keys sends two connect attempts and keeps the one that succeeds', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Office', key: _keyA, appKey: _appKeyA);
      await store.addHost(name: 'Garage', key: _keyB, appKey: _appKeyB);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // The engine finds the typed host only under the second application key. The first try
      // ends with HB-LOOKUP-TIMEOUT; the controller then closes the host and tries the next key
      // (spec/ipc.md decided point 5).
      host.onRequest = (request) {
        if (request is ConnectRequest && _b64(request.appKey) != _b64(_appKeyB)) {
          host.deliver(
            encode(
              IpcReply(
                id: request.id,
                ok: false,
                code: 'HB-LOOKUP-TIMEOUT',
                detail: 'the search gave up',
              ),
            ),
          );
        } else if (request is ConnectRequest) {
          host.deliver(
            encode(IpcReply(id: request.id, ok: true, route: 'direct', services: const [ServiceEntry(name: 'ssh', kind: 3)])),
          );
        } else {
          host.deliver(encode(IpcReply(id: request.id, ok: true)));
        }
      };
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();

      final added = await controller.addTypedKey(_typedKey);

      expect(
        host.requests.map((r) => r.runtimeType),
        [ConnectRequest, CloseRequest, ConnectRequest],
        reason:
            'one key at a time (spec/ipc.md decided point 5): connect, close the host after '
            'HB-LOOKUP-TIMEOUT, then connect with the next key',
      );
      final first = host.requests[0] as ConnectRequest;
      final closed = host.requests[1] as CloseRequest;
      final second = host.requests[2] as ConnectRequest;
      expect(first.appKey, _appKeyA, reason: 'keys are tried in the order the store holds them');
      expect(second.appKey, _appKeyB);
      expect(closed.host, first.host, reason: 'the failed try is closed under its own name');

      final connects = host.requestsOf<ConnectRequest>().toList();
      expect(connects, hasLength(2));
      expect(connects.map((c) => c.key), everyElement(_typedKeyNormalized));
      expect(connects.map((c) => _b64(c.appKey)).toSet(), {_b64(_appKeyA), _b64(_appKeyB)});

      final hosts = await store.hosts();
      expect(hosts, hasLength(3), reason: 'only the host that answered is added');
      final kept = hosts.singleWhere((h) => h.key == _typedKeyNormalized);
      expect(added, kept.id);
      expect(kept.name, second.host, reason: 'the saved name is the one the engine has registered');
      final appKeys = await store.appKeys();
      expect(
        appKeys[kept.appKeyIndex],
        _appKeyB,
        reason: 'the application key that connected is saved with the host',
      );
    });

    test('addTypedKey with no held app keys returns null without contacting the engine', () async {
      final store = HostStore(_MemoryBackend());
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();
      final sentBefore = host.sent.length;

      expect(await controller.addTypedKey(_typedKey), isNull);
      expect(host.sent, hasLength(sentBefore));
    });

    testWidgets('connect() does not time out before 75 s, longer than the engine 60 s lookup', (
      tester,
    ) async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      // No answers: the engine's own reply would come at 60 s, and this test does not send it.
      final host = FakeEngineHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);
      await controller.start();

      var finished = false;
      unawaited(
        controller
            .connect(added.id)
            .then<void>(
              (_) {
                finished = true;
              },
              onError: (Object _) {
                finished = true;
              },
            ),
      );
      await tester.pump();

      await tester.pump(const Duration(seconds: 70));
      expect(
        finished,
        isFalse,
        reason:
            'the connect must not time out at 60 s; the engine answers HB-LOOKUP-TIMEOUT at 60 s',
      );

      await tester.pump(const Duration(seconds: 6));
      expect(finished, isTrue, reason: 'the connect ends at its 75 s timeout');
    });

    for (final kind in _kindCases.entries) {
      test('a services event with kind ${kind.key} shows the service as ${kind.value}', () async {
        final store = HostStore(_MemoryBackend());
        final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
        final host = _answeringHost();
        addTearDown(host.close);
        final controller = await _started(host, store);

        host.deliver(
          encode(
            ServicesEvent(
              host: 'Home',
              list: [ServiceEntry(name: 'svc', kind: kind.key)],
              ports: const [],
            ),
          ),
        );
        await pumpEventQueue();

        expect(controller.view(added.id).services.single.kind, kind.value);
      });
    }

    test('the names and kinds of the services are shown after a restart, before any session is up', () async {
      final backend = _MemoryBackend();
      final store = HostStore(backend);
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final first = _answeringHost();
      addTearDown(first.close);
      await _started(first, store);
      first.deliver(
        encode(
          const ServicesEvent(
            host: 'Home',
            list: [ServiceEntry(name: 'ssh', kind: 3), ServiceEntry(name: 'web', kind: 1)],
            ports: [PortBinding(service: 'ssh', port: 2222)],
          ),
        ),
      );
      await pumpEventQueue();

      // The next run of the app: a new controller over the same store, and no session up.
      final second = FakeEngineHost();
      addTearDown(second.close);
      final controller = await _started(second, HostStore(backend));

      final services = controller.view(added.id).services;
      expect([for (final s in services) (s.name, s.kind)], [('ssh', 'tcp'), ('web', 'https')]);
      expect(services.map((s) => s.port), [null, null], reason: 'nothing is bound before a session is up');
    });

    test('the relay request carries the first held application key', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Office', key: _keyA, appKey: _appKeyA);
      await store.addHost(name: 'Garage', key: _keyB, appKey: _appKeyB);
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);

      await _started(host, store);

      final relay = host.requestsOf<RelayRequest>().single;
      expect(_b64(relay.appKey), _b64(_appKeyA), reason: 'the first application key in appKeys() order');
    });

    test('no relay request while no application key is held; connect() sends it with the first host', () async {
      final store = HostStore(_MemoryBackend());
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      expect(host.requestsOf<RelayRequest>(), isEmpty);

      // A host added without the controller, as the add-host screen adds one, is connected next.
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await controller.connect(added.id);

      expect(host.requests.map((r) => r.runtimeType), [RelayRequest, ConnectRequest]);
      expect(_b64(host.requestsOf<RelayRequest>().single.appKey), _b64(_appKeyA));
    });

    test('an error event sets the controller last error code and no host view', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      var notified = false;
      controller.addListener(() => notified = true);

      host.deliver(
        encode(
          const ErrorEvent(code: 'HB-RELAY-REFUSED', detail: 'the relay did not accept this key'),
        ),
      );
      await pumpEventQueue();

      expect(notified, isTrue, reason: 'listeners are told of the error');
      expect(controller.lastErrorCode, 'HB-RELAY-REFUSED');
      expect(controller.view(added.id).lastErrorCode, isNull, reason: 'an error event names no host');
    });

    test('a connect reply with ok false sets that host last error code and throws nothing', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(
        encode(IpcReply(id: request.id, ok: false, code: 'HB-LOOKUP-TIMEOUT', detail: 'the search gave up')),
      );
      final controller = await _started(host, store);

      await controller.connect(added.id);

      expect(controller.view(added.id).lastErrorCode, 'HB-LOOKUP-TIMEOUT');
      expect(controller.lastErrorCode, isNull, reason: 'the reply names its host, so the controller error stays unset');
    });

    test('a host that timed out and is then reached shows its route, no error, and no second connect', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(
        encode(IpcReply(id: request.id, ok: false, code: 'HB-LOOKUP-TIMEOUT', detail: 'the search gave up')),
      );
      final controller = await _started(host, store);
      await controller.connect(added.id);
      expect(controller.view(added.id).lastErrorCode, 'HB-LOOKUP-TIMEOUT');

      // The engine keeps the host registered and retrying, so its route arrives with no new connect.
      host.deliver(encode(const RouteEvent(host: 'Home', route: 'direct')));
      await pumpEventQueue();
      await controller.connect(added.id);

      expect(controller.view(added.id).route, 'direct');
      expect(controller.view(added.id).lastErrorCode, isNull, reason: 'a route that is up clears the error');
      expect(host.requestsOf<ConnectRequest>(), hasLength(1), reason: 'a registered host is not connected twice');
    });

    test('a route that goes down to no session leaves the host with no route', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      host.deliver(encode(const RouteEvent(host: 'Home', route: 'direct')));
      await pumpEventQueue();
      host.deliver(encode(const SessionEvent(host: 'Home', up: false)));
      await pumpEventQueue();

      expect(controller.view(added.id).route, '');
    });

    test('a session that goes down while the engine is looking leaves the route looking', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      host.deliver(encode(const RouteEvent(host: 'Home', route: 'direct')));
      await pumpEventQueue();
      // The engine reports looking before the session down event, as its route manager does on a drop.
      host.deliver(encode(const RouteEvent(host: 'Home', route: 'looking')));
      host.deliver(encode(const SessionEvent(host: 'Home', up: false)));
      await pumpEventQueue();

      expect(controller.view(added.id).route, 'looking', reason: 'a search runs, so the badge stays');
    });

    test('a route looking that arrives after a session down event is shown', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      host.deliver(encode(const RouteEvent(host: 'Home', route: 'direct')));
      await pumpEventQueue();
      host.deliver(encode(const SessionEvent(host: 'Home', up: false)));
      await pumpEventQueue();
      host.deliver(encode(const RouteEvent(host: 'Home', route: 'looking')));
      await pumpEventQueue();

      expect(controller.view(added.id).route, 'looking');
    });

    test('a session that goes down while the route is unreachable leaves the route unreachable', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      host.deliver(encode(const RouteEvent(host: 'Home', route: 'unreachable')));
      await pumpEventQueue();
      host.deliver(encode(const SessionEvent(host: 'Home', up: false)));
      await pumpEventQueue();

      expect(controller.view(added.id).route, 'unreachable');
    });

    test('connect() rethrows a StateError when the engine is not running', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await host.stop();
      await pumpEventQueue();

      await expectLater(controller.connect(added.id), throwsA(isA<StateError>()));
    });

    test('a desync sets HB-IPC-DESYNC, and after the restart the next connect registers the host again', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);

      // A frame that does not decode: the client reports the desync and restarts the engine.
      host.deliver(Uint8List.fromList([0xff]));
      await pumpEventQueue();
      expect(controller.lastErrorCode, 'HB-IPC-DESYNC');

      await controller.connect(added.id);

      expect(host.requestsOf<ConnectRequest>(), hasLength(2), reason: 'a restarted engine has no registration');
      expect(host.calls, ['start', 'stop', 'start']);
    });

    test('a connect reply with ok true applies its route, services and ports as the events do', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(
        encode(
          IpcReply(
            id: request.id,
            ok: true,
            route: 'lan',
            services: const [ServiceEntry(name: 'ssh', kind: 3)],
            ports: const [PortBinding(service: 'ssh', port: 2222)],
          ),
        ),
      );
      final controller = await _started(host, store);

      await controller.connect(added.id);
      await pumpEventQueue();

      final view = controller.view(added.id);
      expect(view.route, 'lan');
      expect(view.services.single.kind, 'tcp');
      expect(view.services.single.port, 2222);
      expect(await store.servicesCache(added.id), ['ssh']);
      expect(await store.serviceKinds(added.id), {'ssh': 3});
      expect(await store.ports(added.id), {'ssh': 2222});
    });

    test('a lan event is saved, and the next connect sends its addresses and port', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      host.deliver(encode(const LanEvent(host: 'Home', addresses: ['192.168.1.30'], port: 4433)));
      await pumpEventQueue();
      await controller.connect(added.id);

      expect(await store.lanAddresses(added.id), ['192.168.1.30']);
      expect(await store.lanPort(added.id), 4433);
      final connect = host.requestsOf<ConnectRequest>().single;
      expect(connect.lan.addresses, ['192.168.1.30']);
      expect(connect.lan.port, 4433);
    });

    test('reconnect closes a registered host, then connects it with the ports the store holds now', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveLan(added.id, ['192.168.1.20']);
      await store.saveLanPort(added.id, 4433);
      await store.savePort(added.id, 'ssh', 2222);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);
      await store.savePort(added.id, 'ssh', 2233);
      final sentBefore = host.requests.length;

      await controller.reconnect(added.id);

      final sent = host.requests.skip(sentBefore).toList();
      expect(sent.map((r) => r.runtimeType), [CloseRequest, ConnectRequest]);
      expect((sent[0] as CloseRequest).host, 'Home', reason: 'the close names the saved host name');
      final connect = sent[1] as ConnectRequest;
      expect(connect.host, 'Home');
      expect(connect.key, _keyA);
      expect(_b64(connect.appKey), _b64(_appKeyA));
      expect(connect.lan.addresses, ['192.168.1.20']);
      expect(connect.lan.port, 4433);
      expect({for (final port in connect.ports) port.service: port.port}, containsPair('ssh', 2233));
      expect(connect.bind, '127.0.0.1');
    });

    test("reconnect waits for the close's reply before it connects", () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);
      final sentBefore = host.requests.length;
      // The close is held: its reply is sent below. Every other request is answered ok.
      host.onRequest = (request) {
        if (request is CloseRequest) return;
        host.deliver(encode(IpcReply(id: request.id, ok: true)));
      };

      final reconnecting = controller.reconnect(added.id);
      await Future<void>.delayed(Duration.zero);

      expect(host.requests.skip(sentBefore).map((r) => r.runtimeType), [CloseRequest]);

      host.deliver(encode(IpcReply(id: host.requestsOf<CloseRequest>().single.id, ok: true)));
      await reconnecting;

      expect(host.requests.skip(sentBefore).map((r) => r.runtimeType), [CloseRequest, ConnectRequest]);
    });

    test('reconnect shows the route, services and ports of the new connect reply, and clears the earlier error', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(
        encode(IpcReply(id: request.id, ok: false, code: 'HB-LOOKUP-TIMEOUT', detail: 'the search gave up')),
      );
      final controller = await _started(host, store);
      await controller.connect(added.id);
      expect(controller.view(added.id).lastErrorCode, 'HB-LOOKUP-TIMEOUT');

      await store.savePort(added.id, 'ssh', 2233);
      host.onRequest = (request) {
        if (request is ConnectRequest) {
          host.deliver(
            encode(
              IpcReply(
                id: request.id,
                ok: true,
                route: 'lan',
                services: const [ServiceEntry(name: 'ssh', kind: 3)],
                ports: const [PortBinding(service: 'ssh', port: 2233)],
              ),
            ),
          );
        } else {
          host.deliver(encode(IpcReply(id: request.id, ok: true)));
        }
      };
      var notified = false;
      controller.addListener(() => notified = true);

      await controller.reconnect(added.id);

      expect(notified, isTrue, reason: 'listeners are told the view changed');
      final view = controller.view(added.id);
      expect(view.route, 'lan');
      expect(view.services.single.name, 'ssh');
      expect(view.services.single.kind, 'tcp');
      expect(view.services.single.port, 2233);
      expect(view.lastErrorCode, isNull, reason: 'a connect that succeeds clears the earlier error');
    });

    test('a host that timed out is closed and connected again, and a connect after that sends nothing', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(
        encode(IpcReply(id: request.id, ok: false, code: 'HB-LOOKUP-TIMEOUT', detail: 'the search gave up')),
      );
      final controller = await _started(host, store);
      await controller.connect(added.id);
      final sentBefore = host.requests.length;
      host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));

      await controller.reconnect(added.id);

      expect(host.requests.skip(sentBefore).map((r) => r.runtimeType), [CloseRequest, ConnectRequest]);
      final afterReconnect = host.requests.length;
      await controller.connect(added.id);
      expect(host.requests, hasLength(afterReconnect), reason: 'the host is registered again after an ok reply');
    });

    test('reconnect closes a host the engine does not have, then connects it', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await controller.reconnect(added.id);

      expect(host.requests.map((r) => r.runtimeType), [CloseRequest, ConnectRequest]);
    });

    test('a connect the engine refuses leaves the host unregistered, so a later connect sends it again', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
      final controller = await _started(host, store);
      await controller.connect(added.id);
      // From here on the engine refuses every connect and answers the rest ok.
      host.onRequest = (request) {
        if (request is ConnectRequest) {
          host.deliver(
            encode(IpcReply(id: request.id, ok: false, code: 'HB-VERSION-MISMATCH', detail: 'the engine versions differ')),
          );
        } else {
          host.deliver(encode(IpcReply(id: request.id, ok: true)));
        }
      };

      await controller.reconnect(added.id);
      expect(controller.view(added.id).lastErrorCode, 'HB-VERSION-MISMATCH');

      final sentBefore = host.requests.length;
      await controller.connect(added.id);
      expect(host.requests.skip(sentBefore).map((r) => r.runtimeType), [ConnectRequest]);
    });

    test('reconnect waits for a connect that is still in flight', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // The reply to the first connect is held: it is sent below.
      var holdFirst = true;
      host.onRequest = (request) {
        if (request is ConnectRequest && holdFirst) {
          holdFirst = false;
          return;
        }
        host.deliver(encode(IpcReply(id: request.id, ok: true)));
      };
      final controller = await _started(host, store);

      final first = controller.connect(added.id);
      // Future.sync keeps a failure of reconnect out of this body, so the first connect still ends
      // before the test does. ignore() marks that failure as handled, so the await at the end reports it once.
      final reconnecting = Future<void>.sync(() => controller.reconnect(added.id))..ignore();
      await Future<void>.delayed(Duration.zero);

      expect(
        host.requests.map((r) => r.runtimeType),
        [ConnectRequest],
        reason: 'a close sent while the first connect waits would race it',
      );

      host.deliver(encode(IpcReply(id: host.requestsOf<ConnectRequest>().single.id, ok: true)));
      await first;
      await reconnecting;

      expect(host.requests.map((r) => r.runtimeType), [ConnectRequest, CloseRequest, ConnectRequest]);
    });

    test('reconnect throws StateError for an id the store does not know, and sends nothing', () async {
      final store = HostStore(_MemoryBackend());
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await expectLater(controller.reconnect('no-such-host'), throwsA(isA<StateError>()));
      expect(host.sent, isEmpty, reason: 'not even a close for the unknown id');
    });

    test('addTypedKey closes each host that times out, and returns null with the last code when none answers', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Office', key: _keyA, appKey: _appKeyA);
      await store.addHost(name: 'Garage', key: _keyB, appKey: _appKeyB);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(
        encode(
          request is CloseRequest
              ? IpcReply(id: request.id, ok: true)
              : IpcReply(id: request.id, ok: false, code: 'HB-LOOKUP-TIMEOUT', detail: 'the search gave up'),
        ),
      );
      final controller = await _started(host, store);

      expect(await controller.addTypedKey(_typedKey), isNull);

      expect(host.requests.map((r) => r.runtimeType), [ConnectRequest, CloseRequest, ConnectRequest, CloseRequest]);
      expect((await store.hosts()).map((h) => h.name), ['Office', 'Garage'], reason: 'nothing is added');
      expect(controller.lastErrorCode, 'HB-LOOKUP-TIMEOUT');
    });

    test('addTypedKey stops at a reply that is not a lookup timeout, closes that host, and keeps its code', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Office', key: _keyA, appKey: _appKeyA);
      await store.addHost(name: 'Garage', key: _keyB, appKey: _appKeyB);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(
        encode(
          request is CloseRequest
              ? IpcReply(id: request.id, ok: true)
              : IpcReply(id: request.id, ok: false, code: 'HB-KEY-INVALID', detail: 'the key is not valid'),
        ),
      );
      final controller = await _started(host, store);

      expect(await controller.addTypedKey(_typedKey), isNull);

      expect(host.requests.map((r) => r.runtimeType), [ConnectRequest, CloseRequest], reason: 'the next key is not tried');
      expect(controller.lastErrorCode, 'HB-KEY-INVALID');
    });

    test('addTypedKey with a key that does not normalize returns null with HB-KEY-INVALID and contacts no one', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      final sentBefore = host.sent.length;

      expect(await controller.addTypedKey('7KQ-M4X'), isNull, reason: 'six symbols');
      expect(controller.lastErrorCode, 'HB-KEY-INVALID');
      expect(host.sent, hasLength(sentBefore));
    });

    test('addTypedKey for a key already added returns that host and sends nothing', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      final sentBefore = host.sent.length;

      expect(await controller.addTypedKey('7kq-m4x-9tr'), added.id);
      expect(host.sent, hasLength(sentBefore));
    });

    test('a lan event that arrives before the connect reply is saved for the host that addTypedKey adds', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Office', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // The engine reports the host's LAN addresses when its search finds the host, before the connect reply.
      host.onRequest = (request) {
        if (request is ConnectRequest) {
          host.deliver(encode(LanEvent(host: request.host, addresses: const ['192.168.1.30'], port: 4433)));
          host.deliver(
            encode(
              IpcReply(id: request.id, ok: true, route: 'lan', services: const [ServiceEntry(name: 'ssh', kind: 3)]),
            ),
          );
        } else {
          host.deliver(encode(IpcReply(id: request.id, ok: true)));
        }
      };
      final controller = await _started(host, store);

      final id = await controller.addTypedKey(_typedKey);
      await pumpEventQueue();

      expect(id, isNotNull);
      expect(await store.lanAddresses(id!), ['192.168.1.30']);
      expect(await store.lanPort(id), 4433);
    });

    test('a relay key saved after start() is sent before the first typed connect', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      // The user sets the relay key after start() ran, so start() sent no relay request.
      await store.saveRelayKey(_relayKey);
      await controller.addTypedKey(_typedKey);

      expect(
        host.requests.map((r) => r.runtimeType),
        [RelayRequest, ConnectRequest],
        reason: 'the relay key must be in place before the dial, which a host behind a NAT needs',
      );
    });

    test('two addTypedKey calls at once run one after the other', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Office', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // Each connect reports the LAN addresses of its own key before its reply, as the engine does.
      host.onRequest = (request) {
        if (request is ConnectRequest) {
          final address = request.key == _typedKeyNormalized ? '10.0.0.1' : '10.0.0.2';
          host.deliver(encode(LanEvent(host: request.host, addresses: [address], port: 4433)));
          host.deliver(encode(IpcReply(id: request.id, ok: true, route: 'direct')));
        } else {
          host.deliver(encode(IpcReply(id: request.id, ok: true)));
        }
      };
      final controller = await _started(host, store);

      final ids = await Future.wait([
        controller.addTypedKey(_typedKey),
        controller.addTypedKey('JKM-NPQ-RST'),
      ]);
      await pumpEventQueue();

      expect(ids[0], isNotNull);
      expect(ids[1], isNotNull);
      expect(ids[1], isNot(ids[0]), reason: 'each call adds its own host');
      expect([for (final connect in host.requestsOf<ConnectRequest>()) connect.host], ['Host', 'Host 2']);
      expect([for (final h in await store.hosts()) h.name], ['Office', 'Host', 'Host 2']);
      expect(await store.lanAddresses(ids[0]!), ['10.0.0.1'], reason: 'the first call keeps its own LAN event');
      expect(await store.lanAddresses(ids[1]!), ['10.0.0.2'], reason: 'the second call keeps its own LAN event');
    });

    test('a failed addTypedKey does not stop the next one', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Office', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      var failedOnce = false;
      host.onRequest = (request) {
        if (request is ConnectRequest && !failedOnce) {
          failedOnce = true;
          throw StateError('engine down');
        }
        host.deliver(encode(IpcReply(id: request.id, ok: true, route: 'direct')));
      };
      final controller = await _started(host, store);

      final first = controller.addTypedKey(_typedKey);
      final second = controller.addTypedKey('JKM-NPQ-RST');

      await expectLater(first, throwsStateError);
      expect(await second, isNotNull, reason: 'the queue moves on after a call that threw');
      expect([for (final h in await store.hosts()) h.name], ['Office', 'Host']);
    });

    test('two connects at once send one relay request', () async {
      final store = HostStore(_MemoryBackend());
      final a = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final b = await store.addHost(name: 'Office', key: _keyB, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await store.saveRelayKey(_relayKey);

      await Future.wait([controller.connect(a.id), controller.connect(b.id)]);

      expect(host.requestsOf<RelayRequest>(), hasLength(1), reason: 'one relay request per engine start');
    });

    test('the relay request is sent again after a desync', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);

      // A frame that does not decode: the client restarts the engine, which has no relay.
      host.deliver(Uint8List.fromList([0xff]));
      await pumpEventQueue();
      await controller.connect(added.id);

      expect(
        host.requests.map((r) => r.runtimeType),
        [RelayRequest, ConnectRequest, RelayRequest, ConnectRequest],
      );
    });

    test('a crash after a desync restart restarts the engine: the client restart renews the crash listener', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);

      // A frame that does not decode: the client restarts the engine itself, not through the controller.
      host.deliver(Uint8List.fromList([0xff]));
      await pumpEventQueue();
      expect(host.calls, ['start', 'stop', 'start'], reason: 'the client restarted the engine');

      // The crash listener must be on the worklet the client started, or this crash goes unnoticed.
      host.crash();
      await pumpEventQueue();

      expect(host.calls, ['start', 'stop', 'start', 'stop', 'start'], reason: 'the crash listener is on the new worklet');
    });

    test('a crash restarts the engine', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      await _started(host, HostStore(_MemoryBackend()));

      host.crash();
      await pumpEventQueue();

      expect(host.calls, ['start', 'stop', 'start']);
    });

    test('after a crash the next connect registers the host again', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);

      host.crash();
      await pumpEventQueue();
      await controller.connect(added.id);

      expect(host.requestsOf<ConnectRequest>(), hasLength(2), reason: 'a restarted engine has no registration');
    });

    test('after a crash the relay request is sent again', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      expect(host.requestsOf<RelayRequest>(), hasLength(1), reason: 'start() sends the relay request');

      host.crash();
      await pumpEventQueue();
      await controller.connect(added.id);

      expect(host.requestsOf<RelayRequest>(), hasLength(2), reason: 'a restarted engine has no relay');
    });

    test('a crash clears the route the host had', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      host.deliver(encode(const RouteEvent(host: 'Home', route: 'direct')));
      await pumpEventQueue();
      expect(controller.view(added.id).route, 'direct');

      host.crash();
      await pumpEventQueue();

      expect(controller.view(added.id).route, '');
    });

    test('a second crash, after the restart, restarts the engine again', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      await _started(host, HostStore(_MemoryBackend()));

      host.crash();
      await pumpEventQueue();
      host.crash();
      await pumpEventQueue();

      expect(host.calls, ['start', 'stop', 'start', 'stop', 'start']);
    });

    test('two crashes before the first restart finishes restart the engine once', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      await _started(host, HostStore(_MemoryBackend()));

      host.crash();
      host.crash();
      await pumpEventQueue();

      expect(host.calls, ['start', 'stop', 'start']);
    });

    test('a restart that fails reports HB-ENGINE-DOWN and does not throw', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, HostStore(_MemoryBackend()));

      host.failNextStart = true;
      host.crash();
      await pumpEventQueue();

      expect(controller.lastErrorCode, 'HB-ENGINE-DOWN');
      expect(host.calls, ['start', 'stop', 'start']);
    });

    test('after a failed restart, a restartEngine that succeeds clears HB-ENGINE-DOWN and the next connect sends connect again', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);
      host.failNextStart = true;
      host.crash();
      await pumpEventQueue();
      expect(controller.lastErrorCode, 'HB-ENGINE-DOWN');

      await controller.restartEngine();
      expect(controller.lastErrorCode, isNull);
      await controller.connect(added.id);

      expect(host.requestsOf<ConnectRequest>(), hasLength(2));
    });

    test('restartEngine() restarts the engine, and the crash listener is renewed', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);

      await controller.restartEngine();
      expect(host.calls, ['start', 'stop', 'start']);
      await controller.connect(added.id);
      expect(host.requestsOf<ConnectRequest>(), hasLength(2), reason: 'a restarted engine has no registration');

      host.crash();
      await pumpEventQueue();

      expect(host.calls, ['start', 'stop', 'start', 'stop', 'start'], reason: 'the crash listener is on the new worklet');
    });

    test('two restartEngine() calls at once run one restart', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, HostStore(_MemoryBackend()));

      await Future.wait([controller.restartEngine(), controller.restartEngine()]);

      expect(host.calls, ['start', 'stop', 'start']);
    });

    test('restartEngine() before start() throws StateError and touches no engine', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, HostStore(_MemoryBackend()));
      addTearDown(controller.dispose);

      Object? error;
      try {
        await controller.restartEngine();
      } catch (e) {
        error = e;
      }

      expect(error, isA<StateError>());
      expect(host.calls, isEmpty);
    });

    test('dispose() stops listening: a crash after it does not restart the engine', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      // The controller is disposed by the test itself, so it is not disposed again at the end.
      final controller = AppController(host, HostStore(_MemoryBackend()));
      await controller.start();
      // A crash while the controller listens restarts the engine. That shows the listener exists, so
      // the crash after dispose can only be ignored because of dispose.
      host.crash();
      await pumpEventQueue();
      expect(host.calls, ['start', 'stop', 'start']);

      controller.dispose();
      host.crash();
      await pumpEventQueue();

      expect(host.calls, ['start', 'stop', 'start'], reason: 'the disposed controller no longer listens');
    });

    test('dispose() while a restart is in flight does not throw', () async {
      final host = _answeringHost();
      addTearDown(host.close);
      // The controller is disposed by the test itself, so it is not disposed again at the end.
      final controller = AppController(host, HostStore(_MemoryBackend()));
      await controller.start();
      // The engine's stop waits on the gate, so the restart that the crash starts stays in flight.
      final gate = host.stopGate = Completer<void>();
      host.crash();
      await pumpEventQueue();
      expect(host.calls, ['start', 'stop'], reason: 'the restart is waiting in stop');

      controller.dispose();
      gate.complete();
      // No error reported by the end of the test is the check: the restart must not throw after dispose.
      await pumpEventQueue();
    });

    // The relay key and Share with my network, which the settings screen sets through the controller
    // (HoleBridge-5vk.13, HoleBridge-5vk.14).

    test('setRelayKey() normalizes the typed key, stores it and sends relay with the first held application key', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.addHost(name: 'Office', key: _keyB, appKey: _appKeyB);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await controller.setRelayKey(_relayTyped);

      expect(await store.relayKey(), _relayTypedNormalized);
      final relay = host.requestsOf<RelayRequest>().single;
      expect(relay.key, _relayTypedNormalized);
      expect(_b64(relay.appKey), _b64(_appKeyA), reason: 'the first application key in appKeys() order');
    });

    test('setRelayKey() sends relay again when one was already sent since the engine started', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await controller.setRelayKey(_relayTyped);

      expect([for (final relay in host.requestsOf<RelayRequest>()) relay.key], [
        _relayKey,
        _relayTypedNormalized,
      ], reason: 'start() sent the stored key, and the new key goes after it');
    });

    test('setRelayKey() with null, empty or blank text clears the stored key and sends an empty relay with a zero application key', () async {
      for (final blank in <String?>[null, '', '   ']) {
        final store = HostStore(_MemoryBackend());
        await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
        await store.saveRelayKey(_relayKey);
        final host = _answeringHost();
        addTearDown(host.close);
        final controller = await _started(host, store);
        host.sent.clear();

        await controller.setRelayKey(blank);

        expect(await store.relayKey(), isNull, reason: 'blank: "$blank"');
        final relay = host.requestsOf<RelayRequest>().single;
        expect(relay.key, '', reason: 'an empty key means no relay (spec/ipc.md)');
        expect(relay.appKey, hasLength(32));
        expect(relay.appKey, everyElement(0));
      }
    });

    test('setRelayKey(null) with no application key held still sends an empty relay', () async {
      final store = HostStore(_MemoryBackend());
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await controller.setRelayKey(null);

      final relay = host.requestsOf<RelayRequest>().single;
      expect(relay.key, '');
      expect(relay.appKey, everyElement(0));
    });

    test('setRelayKey() with a key that does not normalize throws KeyFormatException and stores and sends nothing', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveRelayKey(_relayKey);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      host.sent.clear();

      Object? error;
      try {
        // U is outside the Crockford alphabet.
        await controller.setRelayKey('PQR-STV-WXU');
      } on KeyFormatException catch (e) {
        error = e;
      }

      expect(error, isA<KeyFormatException>());
      expect(host.requests, isEmpty);
      expect(await store.relayKey(), _relayKey, reason: 'the stored key is unchanged');
    });

    test('setRelayKey() with no application key held stores the key and sends nothing; connect() sends it with the first host', () async {
      final store = HostStore(_MemoryBackend());
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await controller.setRelayKey(_relayTyped);

      expect(await store.relayKey(), _relayTypedNormalized);
      expect(host.requests, isEmpty, reason: 'the relay request needs an application key');

      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await controller.connect(added.id);

      expect(host.requests.map((r) => r.runtimeType), [RelayRequest, ConnectRequest]);
      expect(host.requestsOf<RelayRequest>().single.key, _relayTypedNormalized);
    });

    test('setRelayKey() with a relay reply of ok false sets the controller last error code and keeps the key', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) =>
          host.deliver(encode(IpcReply(id: request.id, ok: false, code: 'HB-RELAY-REFUSED')));
      final controller = await _started(host, store);

      await controller.setRelayKey(_relayTyped);

      expect(controller.lastErrorCode, 'HB-RELAY-REFUSED');
      expect(await store.relayKey(), _relayTypedNormalized);
    });

    // setRelayKey returns the relay reply's code, so the settings screen shows only a refusal of this save (HoleBridge-5vk.49).
    test('setRelayKey() returns null when the relay reply is ok, on the key path and on the clear path', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      expect(await controller.setRelayKey(_relayTyped), isNull);
      expect(await controller.setRelayKey(null), isNull);

      expect(host.requestsOf<RelayRequest>(), hasLength(2));
      expect(controller.lastErrorCode, isNull);
    });

    test('setRelayKey() returns the code of a relay reply with ok false, on the key path and on the clear path', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) =>
          host.deliver(encode(IpcReply(id: request.id, ok: false, code: 'HB-RELAY-REFUSED')));
      final controller = await _started(host, store);

      expect(await controller.setRelayKey(_relayTyped), 'HB-RELAY-REFUSED');
      expect(await controller.setRelayKey(null), 'HB-RELAY-REFUSED');

      expect(host.requestsOf<RelayRequest>(), hasLength(2));
      expect(controller.lastErrorCode, 'HB-RELAY-REFUSED');
    });

    test('setRelayKey() returns null and sends nothing when no application key is held', () async {
      final store = HostStore(_MemoryBackend());
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      expect(await controller.setRelayKey(_relayTyped), isNull);

      expect(await store.relayKey(), _relayTypedNormalized, reason: 'the key is stored');
      expect(host.requests, isEmpty, reason: 'the relay request needs an application key');
    });

    test('setRelayKey() while a relay request is in flight sends its own after that one, so the engine ends with the new key', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // The engine does not answer the first relay request until the test says so; it answers the rest at once.
      var holdFirstRelay = true;
      int? heldId;
      host.onRequest = (request) {
        if (request is RelayRequest && holdFirstRelay) {
          holdFirstRelay = false;
          heldId = request.id;
          return;
        }
        host.deliver(encode(IpcReply(id: request.id, ok: true)));
      };
      final controller = await _started(host, store);
      await store.saveRelayKey(_relayKey);

      final connecting = controller.connect(added.id);
      await pumpEventQueue();
      expect(heldId, isNotNull, reason: 'the relay request of the connect is on the wire and unanswered');
      final saving = controller.setRelayKey(_relayTyped);
      await pumpEventQueue();
      expect(host.requestsOf<RelayRequest>(), hasLength(1), reason: 'the new key waits for the request in flight');

      host.deliver(encode(IpcReply(id: heldId!, ok: true)));
      await connecting;
      await saving;

      expect([for (final relay in host.requestsOf<RelayRequest>()) relay.key], [_relayKey, _relayTypedNormalized]);
    });

    test('setShared(true) on a connected host closes it, then connects it again with bind 0.0.0.0', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);
      host.sent.clear();

      await controller.setShared(added.id, true);

      expect(await store.shared(added.id), isTrue);
      // The engine gives a connect for a host it already has HB-USAGE (spec/ipc.md), so: close, then connect.
      expect(host.requests.map((r) => r.runtimeType), [CloseRequest, ConnectRequest]);
      expect((host.requests[0] as CloseRequest).host, 'Home');
      final connect = host.requests[1] as ConnectRequest;
      expect(connect.host, 'Home');
      expect(connect.key, _keyA);
      expect(_b64(connect.appKey), _b64(_appKeyA));
      expect(connect.bind, '0.0.0.0');
    });

    test('setShared(false) on a shared host closes it, then connects it again with bind 127.0.0.1', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveShared(added.id, true);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);
      host.sent.clear();

      await controller.setShared(added.id, false);

      expect(await store.shared(added.id), isFalse);
      expect(host.requests.map((r) => r.runtimeType), [CloseRequest, ConnectRequest]);
      expect((host.requests[0] as CloseRequest).host, 'Home');
      expect((host.requests[1] as ConnectRequest).bind, '127.0.0.1');
    });

    test('setShared() on a host that is not connected only stores the setting; the next connect uses it', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await controller.setShared(added.id, true);

      expect(await store.shared(added.id), isTrue);
      expect(host.requests, isEmpty, reason: 'nothing is registered with the engine, so nothing to rebind');

      await controller.connect(added.id);

      expect(host.requestsOf<ConnectRequest>().single.bind, '0.0.0.0');
    });

    test('connect() binds 0.0.0.0 for a host whose Share with my network is on, and 127.0.0.1 for one whose is off', () async {
      final store = HostStore(_MemoryBackend());
      final shared = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final private = await store.addHost(name: 'Office', key: _keyB, appKey: _appKeyA);
      await store.saveShared(shared.id, true);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await controller.connect(shared.id);
      await controller.connect(private.id);

      expect({for (final connect in host.requestsOf<ConnectRequest>()) connect.host: connect.bind}, {
        'Home': '0.0.0.0',
        'Office': '127.0.0.1',
      });
    });

    test('setShared() for a host that is not stored throws StateError and sends nothing', () async {
      final store = HostStore(_MemoryBackend());
      await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      Object? error;
      try {
        await controller.setShared('no-such-host', true);
      } on StateError catch (e) {
        error = e;
      }

      expect(error, isA<StateError>());
      expect(host.requests, isEmpty);
    });

    test('setShared() waits for a connect in progress, then connects again with the new setting', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      await store.saveShared(added.id, true);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // The engine does not answer the first connect until the test says so; it answers the rest at once.
      var holdFirstConnect = true;
      int? heldId;
      host.onRequest = (request) {
        if (request is ConnectRequest && holdFirstConnect) {
          holdFirstConnect = false;
          heldId = request.id;
          return;
        }
        host.deliver(encode(IpcReply(id: request.id, ok: true)));
      };
      final controller = await _started(host, store);

      final connecting = controller.connect(added.id);
      await pumpEventQueue();
      expect(heldId, isNotNull, reason: 'the first connect, with bind 0.0.0.0, is on the wire and unanswered');
      final turningOff = controller.setShared(added.id, false);
      await pumpEventQueue();
      // A close now would leave the connect in progress to finish after it, and bind 0.0.0.0 against the setting.
      expect(host.requestsOf<CloseRequest>(), isEmpty, reason: 'the host is not closed under its connect');

      host.deliver(encode(IpcReply(id: heldId!, ok: true)));
      await connecting;
      await turningOff;

      expect(host.requests.map((r) => r.runtimeType), [ConnectRequest, CloseRequest, ConnectRequest]);
      expect([for (final connect in host.requestsOf<ConnectRequest>()) connect.bind], ['0.0.0.0', '127.0.0.1']);
    });

    test('two setShared() calls at once run one after the other, and the last one decides the bind', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);
      host.sent.clear();

      await Future.wait([controller.setShared(added.id, true), controller.setShared(added.id, false)]);

      expect(host.requests.map((r) => r.runtimeType), [
        CloseRequest,
        ConnectRequest,
        CloseRequest,
        ConnectRequest,
      ]);
      expect([for (final connect in host.requestsOf<ConnectRequest>()) connect.bind], ['0.0.0.0', '127.0.0.1']);
      expect(await store.shared(added.id), isFalse);
    });

    test('a setShared() whose connect throws leaves the host unregistered, so the next connect sends the new bind', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);
      await controller.connect(added.id);
      host.sent.clear();
      // The engine goes down after the close: the next connect request cannot be sent.
      final answer = host.onRequest!;
      host.onRequest = (request) {
        if (request is ConnectRequest) {
          host.onRequest = answer;
          throw StateError('engine down');
        }
        answer(request);
      };

      Object? error;
      try {
        await controller.setShared(added.id, true);
      } on StateError catch (e) {
        error = e;
      }
      expect(error, isA<StateError>(), reason: 'an engine failure is not caught, as in connect');
      expect(await store.shared(added.id), isTrue);

      host.sent.clear();
      await controller.connect(added.id);

      expect(host.requestsOf<ConnectRequest>().single.bind, '0.0.0.0', reason: 'the host was closed, so it is connected again');
    });

    test('status() sends one StatusRequest for the host name, and returns the route and NAT view of the status event', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) {
        if (request is StatusRequest) host.deliver(encode(_statusEvent(route: 'direct')));
        host.deliver(encode(IpcReply(id: request.id, ok: true)));
      };
      final controller = await _started(host, store);

      final status = await controller.status(added.id);

      expect(host.requestsOf<StatusRequest>().map((r) => r.host), ['Home']);
      expect(status.route, 'direct');
      expect(status.nat?.host, '203.0.113.7');
      expect(status.nat?.port, 4433);
      expect(status.nat?.firewalled, isFalse);
      expect(status.nat?.randomized, isTrue);
    });

    test('status() of a host the engine does not have returns route "" and no NAT view, and throws nothing', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      // The engine answers a status request for a host that is not connected with ok false and HB-USAGE.
      host.onRequest = (request) => host.deliver(
        encode(IpcReply(id: request.id, ok: false, code: 'HB-USAGE', detail: 'the host is not connected')),
      );
      final controller = await _started(host, store);

      final status = await controller.status(added.id);

      expect(status.route, '');
      expect(status.nat, isNull);
      expect(controller.view(added.id).route, '');
      expect(controller.view(added.id).lastErrorCode, isNull, reason: 'a status query is not an error of the host');
    });

    test('a status event with no session up sets the route to "" and returns it', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) {
        if (request is StatusRequest) host.deliver(encode(_statusEvent(route: '')));
        host.deliver(encode(IpcReply(id: request.id, ok: true)));
      };
      final controller = await _started(host, store);
      host.deliver(encode(const RouteEvent(host: 'Home', route: 'direct')));
      await pumpEventQueue();
      expect(controller.view(added.id).route, 'direct');

      final status = await controller.status(added.id);
      await pumpEventQueue();

      expect(status.route, '');
      expect(status.nat?.host, '203.0.113.7', reason: 'the engine sends its NAT view with every status event');
      expect(controller.view(added.id).route, '', reason: 'the host view agrees with the status');
    });

    test('status() of an id the store does not have throws StateError and sends nothing', () async {
      final store = HostStore(_MemoryBackend());
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = await _started(host, store);

      await expectLater(controller.status('no-such-host'), throwsA(isA<StateError>()));

      expect(host.sent, isEmpty);
    });

    test('status() before start() throws StateError, as connect() does', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = _answeringHost();
      addTearDown(host.close);
      final controller = AppController(host, store);
      addTearDown(controller.dispose);

      await expectLater(controller.status(added.id), throwsA(isA<StateError>()));

      expect(host.sent, isEmpty);
    });

    test('the route of the status event reaches controller.view(id).route', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) {
        if (request is StatusRequest) host.deliver(encode(_statusEvent(route: 'lan')));
        host.deliver(encode(IpcReply(id: request.id, ok: true)));
      };
      final controller = await _started(host, store);
      var notified = false;
      controller.addListener(() => notified = true);

      await controller.status(added.id);
      await pumpEventQueue();

      expect(notified, isTrue, reason: 'listeners are told the view changed');
      expect(controller.view(added.id).route, 'lan');
    });

    test('a status event that arrives after its reply is still the one status() returns', () async {
      final store = HostStore(_MemoryBackend());
      final added = await store.addHost(name: 'Home', key: _keyA, appKey: _appKeyA);
      final host = FakeEngineHost();
      addTearDown(host.close);
      host.onRequest = (request) => host.deliver(encode(IpcReply(id: request.id, ok: true)));
      final controller = await _started(host, store);

      final pending = controller.status(added.id);
      // The ok reply has arrived; the status event that carries the answer has not.
      await pumpEventQueue();
      host.deliver(encode(_statusEvent(route: 'relay')));
      final status = await pending;

      expect(status.route, 'relay');
      expect(status.nat?.host, '203.0.113.7');
    });
  });
}

/// A status event for the host 'Home' on [route], with the NAT view the engine reports for every host:
/// the documentation address 203.0.113.7, port 4433, not firewalled and randomized.
StatusEvent _statusEvent({required String route}) => StatusEvent(
  host: 'Home',
  route: route,
  sessions: route.isEmpty ? 0 : 1,
  streams: 0,
  flows: 0,
  bytesIn: 0,
  bytesOut: 0,
  nat: const NatInfo(host: '203.0.113.7', port: 4433, firewalled: false, randomized: true),
);
