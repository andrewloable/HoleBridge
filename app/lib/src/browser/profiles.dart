/// One storage profile per service. The platform implementations use the APIs the M1 spike
/// validated: Android multi-profile WebView, WKWebsiteDataStore(forIdentifier:), WebView2 profiles.
abstract class Profiles {
  /// The same host and service always get the same profile. Different services get different
  /// profiles, so their cookies and storage stay apart.
  Future<Object> profileFor(String hostId, String service);
}

/// A [Profiles] that asks the platform for the store with a given id, creating it on first use.
class PlatformProfiles implements Profiles {
  const PlatformProfiles(this.openStore);

  /// The platform side. Each store id maps to one platform storage profile, and the same id must
  /// always open the same profile. A platform that needs another form of id (a UUID, say) derives
  /// it from this one.
  final Future<Object> Function(String storeId) openStore;

  @override
  Future<Object> profileFor(String hostId, String service) =>
      openStore(_storeId(hostId, service));
}

/// The store id for one host's service. The host id's length comes first, so the pair can be
/// read back unambiguously: ("a/b", "c") and ("a", "b/c") give different ids.
String _storeId(String hostId, String service) =>
    '${hostId.length}:$hostId/$service';
