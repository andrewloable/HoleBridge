import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/browser/cert_trust.dart';
import 'package:holebridge/src/store/host_store.dart';
import 'package:holebridge/src/store/secure_backend.dart';

/// An in-memory SecureBackend. It keeps every entry, so a test can check what reached storage.
class _MemoryBackend implements SecureBackend {
  final Map<String, String> entries = {};

  @override
  Future<String?> read(String k) async => entries[k];

  @override
  Future<void> write(String k, String v) async {
    entries[k] = v;
  }

  @override
  Future<void> delete(String k) async {
    entries.remove(k);
  }
}

// Two fake certificates. Only their bytes are hashed, so they need not be X.509. The bytes are
// the ASCII of 'abc' and 'def'. Their fingerprints are the SHA-256 of those bytes (shasum -a 256),
// upper-case and colon-separated.
final certA = Uint8List.fromList('abc'.codeUnits);
final certB = Uint8List.fromList('def'.codeUnits);
const fingerprintA =
    'BA:78:16:BF:8F:01:CF:EA:41:41:40:DE:5D:AE:22:23:B0:03:61:A3:96:17:7A:9C:B4:10:FF:61:F2:00:15:AD';
const fingerprintB =
    'CB:83:79:AC:20:98:AA:16:50:29:E3:93:8A:51:DA:0B:CE:CF:C0:08:FD:67:95:F4:01:17:86:47:F9:6C:5B:34';

// Valid 9-symbol keys (docs/security.md#the-key). Test values only.
const keyA = '7KQM4X9TR';
const keyB = 'HJKMNPQRS';
final appKey = Uint8List(32);

const service = 'web';

void main() {
  group('CertTrust', () {
    test('a platform-trusted cert is allowed without a pin', () async {
      final store = HostStore(_MemoryBackend());
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      final decision = await trust.decide(host.id, service, certA, true);

      expect(decision, isA<TrustAllow>());
      // The platform trusts it, so nothing is pinned.
      expect(await store.certPins(host.id, service), isNull);
    });

    test('an untrusted cert with no pin asks with its SHA-256 fingerprint', () async {
      final store = HostStore(_MemoryBackend());
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      final decision = await trust.decide(host.id, service, certA, false);

      expect(
        decision,
        isA<TrustAskFirstUse>().having(
          (decision) => decision.fingerprint,
          'fingerprint',
          fingerprintA,
        ),
      );
      // Asking does not pin: a pin is set only by pin(), once the user agrees.
      expect(await store.certPins(host.id, service), isNull);
    });

    test('after pin(), the same cert is allowed', () async {
      final store = HostStore(_MemoryBackend());
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      await trust.pin(host.id, service, certA);

      expect(await store.certPins(host.id, service), fingerprintA);
      expect(
        await trust.decide(host.id, service, certA, false),
        isA<TrustAllow>(),
      );
    });

    test('a different cert later warns with both fingerprints', () async {
      final store = HostStore(_MemoryBackend());
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      await trust.pin(host.id, service, certA);
      final decision = await trust.decide(host.id, service, certB, false);

      expect(
        decision,
        isA<TrustWarnChanged>()
            .having(
              (decision) => decision.oldFingerprint,
              'oldFingerprint',
              fingerprintA,
            )
            .having(
              (decision) => decision.newFingerprint,
              'newFingerprint',
              fingerprintB,
            ),
      );
    });

    // Accepting a changed cert needs an explicit pin(): decide() alone never re-pins.
    test('a changed cert keeps warning until pin() accepts it', () async {
      final store = HostStore(_MemoryBackend());
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      await trust.pin(host.id, service, certA);
      expect(
        await trust.decide(host.id, service, certB, false),
        isA<TrustWarnChanged>(),
      );

      // The warning wrote nothing: the old pin stands, the new cert warns again, and the old
      // cert is still allowed.
      expect(await store.certPins(host.id, service), fingerprintA);
      expect(
        await trust.decide(host.id, service, certB, false),
        isA<TrustWarnChanged>(),
      );
      expect(
        await trust.decide(host.id, service, certA, false),
        isA<TrustAllow>(),
      );
    });

    // The "choice to re-trust" (docs/architecture.md#the-in-app-browser): pin() replaces the pin.
    test('pin() replaces an earlier pin', () async {
      final store = HostStore(_MemoryBackend());
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      await trust.pin(host.id, service, certA);
      await trust.pin(host.id, service, certB);

      expect(await store.certPins(host.id, service), fingerprintB);
      expect(
        await trust.decide(host.id, service, certB, false),
        isA<TrustAllow>(),
      );
      expect(
        await trust.decide(host.id, service, certA, false),
        isA<TrustWarnChanged>()
            .having(
              (decision) => decision.oldFingerprint,
              'oldFingerprint',
              fingerprintB,
            )
            .having(
              (decision) => decision.newFingerprint,
              'newFingerprint',
              fingerprintA,
            ),
      );
    });

    test('unpin() makes it ask again', () async {
      final store = HostStore(_MemoryBackend());
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      await trust.pin(host.id, service, certA);
      await trust.unpin(host.id, service);

      expect(await store.certPins(host.id, service), isNull);
      expect(
        await trust.decide(host.id, service, certA, false),
        isA<TrustAskFirstUse>().having(
          (decision) => decision.fingerprint,
          'fingerprint',
          fingerprintA,
        ),
      );
    });

    // Beyond the five listed cases: a pin covers one service of one host, as the design says
    // ("pins that fingerprint for that service").
    test('a pin covers one service of one host only', () async {
      final store = HostStore(_MemoryBackend());
      final nas = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final cabin = await store.addHost(
        name: 'Cabin',
        key: keyB,
        appKey: appKey,
      );
      final trust = CertTrust(store);

      await trust.pin(nas.id, service, certA);

      // Another service of the same host, and the same service of another host, still ask.
      expect(
        await trust.decide(nas.id, 'ssh', certA, false),
        isA<TrustAskFirstUse>(),
      );
      expect(
        await trust.decide(cabin.id, service, certA, false),
        isA<TrustAskFirstUse>(),
      );
    });

    // Beyond the five listed cases: the host key never names a pin's storage key (or log line).
    test('pin() puts the host key in no storage key', () async {
      final backend = _MemoryBackend();
      final store = HostStore(backend);
      final host = await store.addHost(name: 'NAS', key: keyA, appKey: appKey);
      final trust = CertTrust(store);

      await trust.pin(host.id, service, certA);

      // The pin reached storage through HostStore, so the check below is not vacuous.
      expect(await store.certPins(host.id, service), fingerprintA);
      for (final storageKey in backend.entries.keys) {
        expect(storageKey, isNot(contains(keyA)));
      }
    });
  });
}
