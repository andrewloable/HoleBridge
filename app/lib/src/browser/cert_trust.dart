// Trust on first use for self-signed HTTPS in the in-app browser (docs/architecture.md#the-in-app-browser).
import 'dart:typed_data';

import '../store/host_store.dart';

/// What the browser does with the certificate a service presents.
sealed class TrustDecision {
  const TrustDecision();
}

/// The certificate may be used: the platform trusts it, or it matches the pin of this service.
final class TrustAllow extends TrustDecision {
  const TrustAllow();
}

/// No pin and no platform trust: the browser shows [fingerprint] and asks once.
final class TrustAskFirstUse extends TrustDecision {
  const TrustAskFirstUse(this.fingerprint);

  /// SHA-256 of the certificate's DER, as colon-separated hex.
  final String fingerprint;
}

/// The pinned certificate changed: the browser warns and shows both fingerprints.
final class TrustWarnChanged extends TrustDecision {
  const TrustWarnChanged(this.oldFingerprint, this.newFingerprint);

  /// The fingerprint that is pinned.
  final String oldFingerprint;

  /// The fingerprint of the certificate the service presents now.
  final String newFingerprint;
}

/// Trust on first use, per host and service. Pins are kept in the [HostStore] given to the
/// constructor, one per service of a host.
class CertTrust {
  CertTrust(HostStore store);

  /// Decides what to do with [certDer], the certificate of [service] on [hostId]. A platform-trusted
  /// certificate is allowed without a pin. Otherwise a matching pin allows it, a different pin warns,
  /// and no pin asks with the fingerprint. Does not write a pin.
  Future<TrustDecision> decide(
    String hostId,
    String service,
    Uint8List certDer,
    bool platformTrusted,
  ) async {
    throw UnimplementedError();
  }

  /// Pins the fingerprint of [certDer] for [service] on [hostId], replacing any earlier pin.
  Future<void> pin(String hostId, String service, Uint8List certDer) async {
    throw UnimplementedError();
  }

  /// Removes the pin of [service] on [hostId], so the next certificate asks again.
  Future<void> unpin(String hostId, String service) async {
    throw UnimplementedError();
  }
}
