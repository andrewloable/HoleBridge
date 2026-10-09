import 'package:flutter_secure_storage/flutter_secure_storage.dart';

/// The secure storage the host store writes to: string values under string keys
/// (docs/architecture.md#state-status-and-logs). The production backend is the platform's secure
/// storage; tests inject an in-memory fake. Nothing else in the app may keep these values.
abstract class SecureBackend {
  Future<String?> read(String k);

  Future<void> write(String k, String v);

  Future<void> delete(String k);
}

/// The production backend: the platform's secure storage (Keychain on Apple platforms, Keystore-backed
/// storage on Android, the desktop equivalents).
///
/// On macOS it uses the legacy Keychain. The data-protection Keychain needs a keychain-access-groups
/// entitlement and a provisioning profile, and this app shares nothing with another app of its own.
class SecureStorageBackend implements SecureBackend {
  SecureStorageBackend([FlutterSecureStorage? storage])
    : _storage =
          storage ??
          const FlutterSecureStorage(mOptions: MacOsOptions(usesDataProtectionKeychain: false));

  final FlutterSecureStorage _storage;

  @override
  Future<String?> read(String k) => _storage.read(key: k);

  @override
  Future<void> write(String k, String v) => _storage.write(key: k, value: v);

  @override
  Future<void> delete(String k) => _storage.delete(key: k);
}
