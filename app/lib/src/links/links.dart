// Key and handoff links (docs/security.md#the-application-key, docs/architecture.md#adding-a-host-to-a-tv-handoff).
//
// A key link is https://<host>/k#<key>.<application key>. A handoff link is
// https://<host>/h#1.<public key>.<secret>.<port>.<IPv4 addresses>.
import 'dart:typed_data';

import '../errors.g.dart';
import '../keys/normalize.dart';

/// A link the app can open: a key link or a handoff link.
sealed class AppLink {
  const AppLink();
}

/// A key link: the host's 9-symbol key and the deployment's application key.
class KeyLink extends AppLink {
  KeyLink(this.key, this.appKey);

  final String key;
  final Uint8List appKey;
}

/// A handoff link: one-time X25519 public key, one-time secret, port and the TV's LAN addresses.
class HandoffLink extends AppLink {
  HandoffLink(this.publicKey, this.secret, this.port, this.addresses);

  final Uint8List publicKey;
  final Uint8List secret;
  final int port;
  final List<String> addresses;
}

/// Parses a key or handoff link from any host. Throws [FormatException] for any other link.
///
/// The path picks the kind: /k is a key link and /h a handoff link. A key-link fault carries its
/// catalog code and detail, as internal/links reports it. Handoff faults have no code, because the
/// docs define none. No message holds the link or any part of it.
AppLink parseAppLink(String text) {
  final uri = Uri.tryParse(text);
  final path = _typedPath(text);
  if (uri != null && path == '/k') return _parseKey(uri, text);
  if (uri != null && path == '/h') return _parseHandoff(uri);
  throw _keyFault('HB-KEY-INVALID', 'link');
}

/// The path as typed, with its percent escapes decoded, as internal/links reads it. Uri.path removes
/// dot segments, so /./k would pass as /k; net/url keeps them and rejects the link. Null when an
/// escape is not UTF-8, which internal/links also rejects.
String? _typedPath(String text) {
  final raw = _rawPath.firstMatch(text)?.group(1) ?? '';
  try {
    return Uri.decodeComponent(raw);
  } on FormatException {
    return null;
  }
}

/// The text after the scheme and authority and before any query or fragment.
final _rawPath = RegExp(r'^[a-zA-Z][a-zA-Z0-9+.-]*://[^/?#]*([^?#]*)');

/// Builds the key link base + "/k#" + key + "." + the application key as 64 lowercase hex digits.
String buildKeyLink(String base, String key, Uint8List appKey) {
  if (appKey.length != 32) throw ArgumentError('an application key is 32 bytes');
  return '$base/k#$key.${_hex(appKey)}';
}

/// A key-link fault: the catalog code, its problem and the detail, as internal/errs formats it.
FormatException _keyFault(String code, String detail) =>
    FormatException('$code: ${errorCatalog[code]!.problem} ($detail)');

AppLink _parseKey(Uri uri, String text) {
  if (!_isPlainHttps(uri) || _malformedText.hasMatch(text)) throw _keyFault('HB-KEY-INVALID', 'link');
  final fragment = _decodeFragment(uri.fragment);
  final dot = fragment.indexOf('.');
  if (dot < 0) throw _keyFault('HB-KEY-INVALID', 'link');
  return KeyLink(_normalize(fragment.substring(0, dot)), _appKey(fragment.substring(dot + 1)));
}

/// A percent sign that does not start an escape. net/url refuses it, so internal/links gives the
/// key-link fault. The check runs on the text as typed, because Uri turns such a sign into an escape
/// of its own and the fault would be lost.
final _malformedText = RegExp('%(?![0-9A-Fa-f]{2})');

/// The fragment with every percent escape decoded, as internal/links reads it (net/url decodes the
/// fragment). Dart keeps the escapes of reserved characters, so %20 would otherwise reach the key
/// part as three characters. Escapes that are not UTF-8 are a key-link fault for the link.
String _decodeFragment(String fragment) {
  try {
    return Uri.decodeComponent(fragment);
  } on FormatException {
    throw _keyFault('HB-KEY-INVALID', 'link');
  }
}

String _normalize(String keyPart) {
  try {
    return normalizeKey(keyPart);
  } on KeyFormatException catch (error) {
    throw _keyFault('HB-KEY-INVALID', error.reason);
  }
}

AppLink _parseHandoff(Uri uri) {
  final fields = _isPlainHttps(uri) ? _handoff.firstMatch(uri.fragment) : null;
  final port = int.tryParse(fields?[3] ?? '') ?? 0;
  if (fields == null || port < 1 || port > 65535) {
    throw const FormatException('handoff link is not valid');
  }
  return HandoffLink(_bytes(fields[1]!), _bytes(fields[2]!), port, fields[4]!.split(','));
}

bool _isPlainHttps(Uri uri) => uri.scheme == 'https' && uri.host.isNotEmpty && !uri.hasQuery;

/// The application key of a key link: 64 lowercase hex digits. Faults are reported in the order
/// internal/keys checks them: uppercase, then length, then character.
Uint8List _appKey(String hex) {
  if (_upperHex.hasMatch(hex)) throw _keyFault('HB-APPKEY-INVALID', 'uppercase');
  if (hex.length != 64) throw _keyFault('HB-APPKEY-INVALID', 'length');
  if (!_lowerHex.hasMatch(hex)) throw _keyFault('HB-APPKEY-INVALID', 'character');
  return _bytes(hex);
}

Uint8List _bytes(String hex) => Uint8List.fromList([
  for (var i = 0; i < hex.length; i += 2) int.parse(hex.substring(i, i + 2), radix: 16),
]);

String _hex(Uint8List bytes) => [for (final b in bytes) b.toRadixString(16).padLeft(2, '0')].join();

final _upperHex = RegExp('[A-F]');
final _lowerHex = RegExp('^[0-9a-f]+\$');

/// One IPv4 address: four decimal octets from 0 to 255, without leading zeros.
const _octet = r'(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)';
final _ipv4 = '$_octet(?:\\.$_octet){3}';

/// A handoff fragment: version 1, the public key (64 hex digits), the secret (32), the port and
/// the comma-separated IPv4 addresses.
final _handoff = RegExp(
  r'^1\.([0-9a-f]{64})\.([0-9a-f]{32})\.([0-9]{1,5})\.(' + _ipv4 + r'(?:,' + _ipv4 + r')*)$',
);
