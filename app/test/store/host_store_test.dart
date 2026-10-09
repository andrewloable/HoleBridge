import 'dart:io';
import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';

/// An in-memory SecureBackend. It keeps every entry, so a test can check what reached storage.
class MemoryBackend implements SecureBackend {
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

/// Decodes a hex string from the vectors to bytes.
Uint8List bytesOf(String hex) => Uint8List.fromList([
  for (var i = 0; i < hex.length; i += 2) int.parse(hex.substring(i, i + 2), radix: 16),
]);

// Application keys from spec/vectors/links.json, and key values that are valid 9-symbol keys.
final appKeyA = bytesOf('000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f');
final appKeyB = bytesOf('ff' * 32);
const keyA = '7KQM4X9TR';
const keyB = 'HJKMNPQRS';

// Certificate pins are opaque strings to the store; these stand in for SHA-256 fingerprints.
final pinA = 'a' * 64;
final pinB = 'b' * 64;
final pinC = 'c' * 64;

void main() {
  group('HostStore', () {
    test(
      'addHost then hosts() returns it with the normalized key; the same key twice is refused',
      () async {
        final backend = MemoryBackend();
        final store = HostStore(backend);

        final host = await store.addHost(name: 'Home', key: '7kq-m4x-9tr', appKey: appKeyA);

        final hosts = await store.hosts();
        expect(hosts, hasLength(1));
        expect(hosts.single.id, host.id);
        expect(hosts.single.name, 'Home');
        expect(hosts.single.key, keyA);
        expect(host.id, isNotEmpty);

        // The same key typed another way is the same key: it is refused, and the error does not name it.
        await expectLater(
          store.addHost(name: 'Again', key: '7KQ M4X 9TR', appKey: appKeyA),
          throwsA(
            isA<StateError>().having(
              (error) => error.toString(),
              'message',
              isNot(anyOf(contains('7KQ'), contains(keyA))),
            ),
          ),
        );
        expect(await store.hosts(), hasLength(1));

        // The host is in the backend, not in memory: a new store over the same backend sees it.
        expect(await HostStore(backend).hosts(), hasLength(1));
      },
    );

    test('two hosts sharing an application key keep one entry in appKeys()', () async {
      final store = HostStore(MemoryBackend());

      final home = await store.addHost(name: 'Home', key: keyA, appKey: appKeyA);
      final office = await store.addHost(name: 'Office', key: keyB, appKey: appKeyA);

      final keys = await store.appKeys();
      expect(keys, hasLength(1));
      expect(keys.single, equals(appKeyA));
      expect(home.appKeyIndex, 0);
      expect(office.appKeyIndex, 0);

      // A host with a different application key adds a second entry, and points at it.
      final other = await store.addHost(name: 'Other', key: '0123ABCDE', appKey: appKeyB);
      final both = await store.appKeys();
      expect(both, hasLength(2));
      expect(both[other.appKeyIndex], equals(appKeyB));
    });

    test('removeHost deletes its ports, pins, services cache and LAN addresses', () async {
      final backend = MemoryBackend();
      final store = HostStore(backend);
      final gone = await store.addHost(name: 'Gone', key: keyA, appKey: appKeyA);
      final kept = await store.addHost(name: 'Kept', key: keyB, appKey: appKeyA);
      for (final id in [gone.id, kept.id]) {
        await store.savePort(id, 'web', 8080);
        await store.savePin(id, 'web', pinA);
        await store.saveServices(id, ['web']);
        await store.saveLan(id, ['192.0.2.10']);
      }

      await store.removeHost(gone.id);

      expect((await store.hosts()).map((host) => host.id), [kept.id]);
      expect(await store.ports(gone.id), isEmpty);
      expect(await store.certPins(gone.id, 'web'), isNull);
      expect(await store.servicesCache(gone.id), isEmpty);
      expect(await store.lanAddresses(gone.id), isEmpty);
      // No stored entry, key or value, still names the removed host.
      final leftovers = backend.entries.entries.where(
        (entry) => entry.key.contains(gone.id) || entry.value.contains(gone.id),
      );
      expect(leftovers, isEmpty);
      // The other host keeps its state.
      expect(await store.ports(kept.id), {'web': 8080});
      expect(await store.certPins(kept.id, 'web'), pinA);
      expect(await store.servicesCache(kept.id), ['web']);
      expect(await store.lanAddresses(kept.id), ['192.0.2.10']);
    });

    test('savePort and ports round-trip per host and service', () async {
      final backend = MemoryBackend();
      final store = HostStore(backend);
      final a = await store.addHost(name: 'A', key: keyA, appKey: appKeyA);
      final b = await store.addHost(name: 'B', key: keyB, appKey: appKeyA);

      await store.savePort(a.id, 'web', 8080);
      await store.savePort(a.id, 'ssh', 2222);
      await store.savePort(b.id, 'web', 9090);

      expect(await store.ports(a.id), {'web': 8080, 'ssh': 2222});
      expect(await store.ports(b.id), {'web': 9090});
      // Stored, not held in memory: a new store over the same backend reads the same ports.
      expect(await HostStore(backend).ports(a.id), {'web': 8080, 'ssh': 2222});
    });

    test('cert pins are per host and service, and removable', () async {
      final store = HostStore(MemoryBackend());
      final a = await store.addHost(name: 'A', key: keyA, appKey: appKeyA);
      final b = await store.addHost(name: 'B', key: keyB, appKey: appKeyA);

      await store.savePin(a.id, 'web', pinA);
      await store.savePin(a.id, 'ssh', pinB);
      await store.savePin(b.id, 'web', pinC);
      expect(await store.certPins(a.id, 'web'), pinA);
      expect(await store.certPins(a.id, 'ssh'), pinB);
      expect(await store.certPins(b.id, 'web'), pinC);

      await store.removePin(a.id, 'web');
      expect(await store.certPins(a.id, 'web'), isNull);
      expect(await store.certPins(a.id, 'ssh'), pinB);
      expect(await store.certPins(b.id, 'web'), pinC);
    });

    test('relay key round-trips and can be cleared', () async {
      final backend = MemoryBackend();
      final store = HostStore(backend);
      expect(await store.relayKey(), isNull);

      await store.saveRelayKey(keyA);
      expect(await store.relayKey(), keyA);
      expect(await HostStore(backend).relayKey(), keyA);

      await store.saveRelayKey(null);
      expect(await store.relayKey(), isNull);
    });

    test('nothing reaches SharedPreferences: a write goes to the secure backend, and no lib file imports it', () async {
      final backend = MemoryBackend();
      await HostStore(backend).addHost(name: 'Home', key: keyA, appKey: appKeyA);
      expect(backend.entries, isNotEmpty);

      final importers = Directory('lib')
          .listSync(recursive: true)
          .whereType<File>()
          .where((file) => file.path.endsWith('.dart'))
          .where((file) => file.readAsStringSync().contains('package:shared_preferences'))
          .map((file) => file.path);
      expect(importers, isEmpty);
    });
  });
}
