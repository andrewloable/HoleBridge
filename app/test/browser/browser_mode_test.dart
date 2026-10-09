import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/browser/browser_mode.dart';
import 'package:holebridge/src/browser/profiles.dart';

/// The fake platform side: one store object per store id, kept for the life of the fake, as the
/// platform keeps one storage profile per identifier.
class _FakeStore {
  _FakeStore(this.storeId);

  final String storeId;
}

Future<Object> Function(String storeId) _fakePlatform() {
  final stores = <String, _FakeStore>{};
  return (storeId) async => stores.putIfAbsent(storeId, () => _FakeStore(storeId));
}

void main() {
  group('modeFor', () {
    test('returns separate when the WebView keeps storage apart per service', () {
      const caps = PlatformCaps(
        hasWebView: true,
        perServiceStorage: true,
        isTv: false,
        isLinuxDesktop: false,
      );

      expect(modeFor(caps), BrowserMode.separate);
    });

    test('returns shared when only a WebView exists', () {
      const caps = PlatformCaps(
        hasWebView: true,
        perServiceStorage: false,
        isTv: false,
        isLinuxDesktop: false,
      );

      expect(modeFor(caps), BrowserMode.shared);
    });

    test('returns system on Linux desktop, which has no WebView', () {
      const caps = PlatformCaps(
        hasWebView: false,
        perServiceStorage: false,
        isTv: false,
        isLinuxDesktop: true,
      );

      expect(modeFor(caps), BrowserMode.system);
    });

    test('returns addressOnly on a TV without a WebView', () {
      const caps = PlatformCaps(
        hasWebView: false,
        perServiceStorage: false,
        isTv: true,
        isLinuxDesktop: false,
      );

      expect(modeFor(caps), BrowserMode.addressOnly);
    });

    test(
      'returns system when there is no WebView and the device is not a TV or Linux desktop',
      () {
        const caps = PlatformCaps(
          hasWebView: false,
          perServiceStorage: false,
          isTv: false,
          isLinuxDesktop: false,
        );

        expect(modeFor(caps), BrowserMode.system);
      },
    );

    test('returns separate on a TV that has a WebView', () {
      const caps = PlatformCaps(
        hasWebView: true,
        perServiceStorage: true,
        isTv: true,
        isLinuxDesktop: false,
      );

      expect(modeFor(caps), BrowserMode.separate);
    });
  });

  group('profileFor', () {
    test(
      'returns the same profile for the same host and service, and different ones for '
      'different services',
      () async {
        final profiles = PlatformProfiles(_fakePlatform());

        final first = await profiles.profileFor('host-a', 'web');
        final again = await profiles.profileFor('host-a', 'web');
        final other = await profiles.profileFor('host-a', 'files');

        expect(again, same(first));
        expect(other, isNot(same(first)));
      },
    );

    test('gives two hosts with a service of the same name different profiles', () async {
      final profiles = PlatformProfiles(_fakePlatform());

      final hostA = await profiles.profileFor('host-a', 'web');
      final hostB = await profiles.profileFor('host-b', 'web');
      final hostBAgain = await profiles.profileFor('host-b', 'web');

      expect(hostB, isNot(same(hostA)));
      expect(hostBAgain, same(hostB));
    });

    test('a slash in a name does not merge two pairs', () async {
      final profiles = PlatformProfiles(_fakePlatform());

      final slashInHost = await profiles.profileFor('a/b', 'c');
      final slashInService = await profiles.profileFor('a', 'b/c');

      expect(slashInService, isNot(same(slashInHost)));
    });
  });

  group('login-collision warning', () {
    test('is set in shared mode', () {
      expect(warnsAboutLoginCollisions(BrowserMode.shared), isTrue);
    });

    test('is not set in separate mode', () {
      expect(warnsAboutLoginCollisions(BrowserMode.separate), isFalse);
    });

    test('is set in system mode', () {
      expect(warnsAboutLoginCollisions(BrowserMode.system), isTrue);
    });

    test('is not set in addressOnly mode', () {
      expect(warnsAboutLoginCollisions(BrowserMode.addressOnly), isFalse);
    });
  });
}
