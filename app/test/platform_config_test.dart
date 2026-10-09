import 'dart:io';

import 'package:flutter_test/flutter_test.dart';

// flutter test runs with app/ as the working directory.
String _read(String path) => File(path).readAsStringSync();

// The text of a plist string value, or null when the key is missing.
String? _plistString(String plist, String key) {
  final match = RegExp('<key>$key</key>\\s*<string>([^<]*)</string>').firstMatch(plist);
  return match?.group(1);
}

bool _plistTrue(String plist, String key) => RegExp('<key>$key</key>\\s*<true/>').hasMatch(plist);

bool _plistFalse(String plist, String key) => RegExp('<key>$key</key>\\s*<false/>').hasMatch(plist);

// The text between <name> and </name>, or '' when the element is missing.
String _section(String xml, String name) =>
    RegExp('<$name>([\\s\\S]*?)</$name>').firstMatch(xml)?.group(1) ?? '';

void main() {
  const androidManifest = 'android/app/src/main/AndroidManifest.xml';
  const networkConfig = 'android/app/src/main/res/xml/network_security_config.xml';
  const dataExtractionRules = 'android/app/src/main/res/xml/data_extraction_rules.xml';
  const fullBackupContent = 'android/app/src/main/res/xml/full_backup_content.xml';
  // The shared preferences files flutter_secure_storage 11.2.0 uses with the default options
  // (FlutterSecureStorageConfig.java). They hold ciphertext wrapped by a Keystore key that is never
  // backed up, so a restore cannot read them.
  const secureStoragePrefs = [
    'FlutterSecureStorage',
    'FlutterSecureKeyStorage',
    'FlutterSecureStorageConfiguration',
  ];
  // The worklet's store, excluded by flutter_pear_bare; the app's own rules must repeat them.
  const pearStore = ['pear-corestore', 'pear-bulk'];
  const iosPlist = 'ios/Runner/Info.plist';
  const macosPlist = 'macos/Runner/Info.plist';
  const debugEntitlements = 'macos/Runner/DebugProfile.entitlements';
  const releaseEntitlements = 'macos/Runner/Release.entitlements';

  group('Android', () {
    test('cleartext is off by default and on only for 127.0.0.1', () {
      final xml = _read(networkConfig);
      expect(
        RegExp(r'<base-config\s+cleartextTrafficPermitted="false"\s*/>').hasMatch(xml),
        isTrue,
        reason: 'the base config must forbid cleartext',
      );
      expect(
        RegExp('cleartextTrafficPermitted="true"').allMatches(xml),
        hasLength(1),
        reason: 'exactly one domain config may permit cleartext',
      );
      final permitted = RegExp(
        r'<domain-config\s+cleartextTrafficPermitted="true">([\s\S]*?)</domain-config>',
      ).firstMatch(xml);
      expect(permitted, isNotNull);
      final domains = RegExp(r'<domain[^>]*>([^<]+)</domain>')
          .allMatches(permitted!.group(1)!)
          .map((m) => m.group(1))
          .toList();
      expect(domains, ['127.0.0.1']);
    });

    test('the manifest uses the config and does not allow cleartext app-wide', () {
      final manifest = _read(androidManifest);
      expect(manifest, contains('android:networkSecurityConfig="@xml/network_security_config"'));
      expect(manifest, isNot(contains('usesCleartextTraffic')));
    });

    test('INTERNET and CAMERA are declared, and the camera is optional', () {
      final manifest = _read(androidManifest);
      expect(manifest, contains('<uses-permission android:name="android.permission.INTERNET"/>'));
      expect(manifest, contains('<uses-permission android:name="android.permission.CAMERA"/>'));
      expect(
        manifest,
        contains('<uses-feature android:name="android.hardware.camera" android:required="false"/>'),
      );
    });

    test(
      'backup is off, and the secure storage prefs stay out of cloud backup and device transfer',
      () {
        final manifest = _read(androidManifest);
        expect(manifest, contains('xmlns:tools="http://schemas.android.com/tools"'));
        expect(manifest, contains('android:allowBackup="false"'));
        expect(manifest, contains('android:dataExtractionRules="@xml/data_extraction_rules"'));
        expect(manifest, contains('android:fullBackupContent="@xml/full_backup_content"'));
        expect(
          manifest,
          contains('tools:replace="android:dataExtractionRules,android:fullBackupContent"'),
          reason:
              'flutter_pear_bare declares the same two attributes, so the merge must replace them',
        );

        // Device-to-device transfer ignores allowBackup on API 31+, so its section needs the excludes.
        final rules = _read(dataExtractionRules);
        for (final section in ['cloud-backup', 'device-transfer']) {
          final body = _section(rules, section);
          expect(body, isNotEmpty, reason: 'data extraction rules need a $section section');
          for (final pref in secureStoragePrefs) {
            expect(
              body,
              contains('<exclude domain="sharedpref" path="$pref.xml"/>'),
              reason: '$section must exclude $pref',
            );
          }
          for (final path in pearStore) {
            expect(
              body,
              contains('<exclude domain="file" path="$path"/>'),
              reason: '$section must keep $path out',
            );
          }
        }

        final full = _read(fullBackupContent);
        for (final pref in secureStoragePrefs) {
          expect(
            full,
            contains('<exclude domain="sharedpref" path="$pref.xml"/>'),
            reason: 'full backup must exclude $pref',
          );
        }
        for (final path in pearStore) {
          expect(full, contains('<exclude domain="file" path="$path"/>'));
        }
      },
    );
  });

  group('iOS', () {
    test('plain HTTP is allowed for local networking only', () {
      final plist = _read(iosPlist);
      expect(_plistTrue(plist, 'NSAllowsLocalNetworking'), isTrue);
      expect(plist, isNot(contains('NSAllowsArbitraryLoads')));
      expect(plist, isNot(contains('NSExceptionDomains')));
    });

    test('the local network and camera prompts have a reason', () {
      final plist = _read(iosPlist);
      expect(_plistString(plist, 'NSLocalNetworkUsageDescription'), isNotEmpty);
      expect(_plistString(plist, 'NSCameraUsageDescription'), isNotEmpty);
    });
  });

  group('macOS', () {
    test('plain HTTP is allowed for local networking only', () {
      final plist = _read(macosPlist);
      expect(_plistTrue(plist, 'NSAllowsLocalNetworking'), isTrue);
      expect(plist, isNot(contains('NSAllowsArbitraryLoads')));
      expect(plist, isNot(contains('NSExceptionDomains')));
    });

    test('the local network prompt has a reason', () {
      final plist = _read(macosPlist);
      expect(_plistString(plist, 'NSLocalNetworkUsageDescription'), isNotEmpty);
    });

    test('both entitlements files keep the sandbox off and allow network client and server', () {
      for (final path in [debugEntitlements, releaseEntitlements]) {
        final plist = _read(path);
        expect(_plistFalse(plist, 'com.apple.security.app-sandbox'), isTrue, reason: path);
        expect(_plistTrue(plist, 'com.apple.security.network.client'), isTrue, reason: path);
        expect(_plistTrue(plist, 'com.apple.security.network.server'), isTrue, reason: path);
      }
    });
  });
}
