import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/links/links.dart';

import '../helpers/vectors.dart';

/// Decodes a hex string from the vectors to bytes.
Uint8List bytesOf(String hex) => Uint8List.fromList([
  for (var i = 0; i < hex.length; i += 2) int.parse(hex.substring(i, i + 2), radix: 16),
]);

/// What parseAppLink throws for [link], or null when it returns a link.
Object? thrownBy(String link) {
  try {
    parseAppLink(link);
    return null;
  } catch (error) {
    return error;
  }
}

/// The code and detail that internal/links gives a key-link fault (docs/errors.md), keyed by the
/// start of the vector's reason. Handoff faults have no code in the docs, so they are not listed.
const keyLinkFaults = <String, (String, String)>{
  'wrong path': ('HB-KEY-INVALID', 'link'),
  'missing fragment': ('HB-KEY-INVALID', 'link'),
  'short application key': ('HB-APPKEY-INVALID', 'length'),
  'non-hex': ('HB-APPKEY-INVALID', 'character'),
  'extra field': ('HB-APPKEY-INVALID', 'length'),
  'trailing %0A': ('HB-APPKEY-INVALID', 'length'),
  'malformed escape': ('HB-KEY-INVALID', 'link'),
  'dot segment': ('HB-KEY-INVALID', 'link'),
};

MapEntry<String, (String, String)>? faultFor(String reason) {
  for (final fault in keyLinkFaults.entries) {
    if (reason.startsWith(fault.key)) return fault;
  }
  return null;
}

void main() {
  final vectors = loadVector('links.json');
  final base = vectors['base'] as String;
  final keyLinks = (vectors['keyLinks'] as List).cast<Map<String, dynamic>>();
  final parse = (vectors['parse'] as List).cast<Map<String, dynamic>>();
  final handoffLinks = (vectors['handoffLinks'] as List).cast<Map<String, dynamic>>();
  final invalid = (vectors['invalid'] as List).cast<Map<String, dynamic>>();

  group('parseAppLink reads every key link in links.json', () {
    for (var i = 0; i < keyLinks.length; i++) {
      final entry = keyLinks[i];
      test('keyLinks[$i] parses to its key and application key', () {
        expect(
          parseAppLink(entry['link'] as String),
          isA<KeyLink>()
              .having((link) => link.key, 'key', entry['key'])
              .having((link) => link.appKey, 'appKey', equals(bytesOf(entry['appKey'] as String))),
        );
      });
    }

    for (var i = 0; i < parse.length; i++) {
      final entry = parse[i];
      test('parse[$i] (forgiving input) parses to the expected key and application key', () {
        expect(
          parseAppLink(entry['link'] as String),
          isA<KeyLink>()
              .having((link) => link.key, 'key', entry['expectKey'])
              .having(
                (link) => link.appKey,
                'appKey',
                equals(bytesOf(entry['expectAppKey'] as String)),
              ),
        );
      });
    }
  });

  group('parseAppLink reads every handoff link in links.json', () {
    for (var i = 0; i < handoffLinks.length; i++) {
      final entry = handoffLinks[i];
      test('handoffLinks[$i] parses to its fields', () {
        expect(
          parseAppLink(entry['link'] as String),
          isA<HandoffLink>()
              .having(
                (link) => link.publicKey,
                'publicKey',
                equals(bytesOf(entry['publicKey'] as String)),
              )
              .having((link) => link.secret, 'secret', equals(bytesOf(entry['secret'] as String)))
              .having((link) => link.port, 'port', entry['port'])
              .having(
                (link) => link.addresses,
                'addresses',
                equals((entry['addresses'] as List).cast<String>()),
              ),
        );
      });
    }
  });

  group('parseAppLink throws FormatException for every invalid link in links.json', () {
    for (var i = 0; i < invalid.length; i++) {
      final link = invalid[i]['link'] as String;
      final reason = invalid[i]['reason'] as String;
      test('invalid[$i] ($reason) throws FormatException', () {
        final error = thrownBy(link);
        expect(error, isA<FormatException>());
        final formatError = error as FormatException;

        // The error never echoes the link, and so never its fragment, which holds the key or secret.
        expect(formatError.toString(), isNot(contains(link)));
        final fragment = link.substring(link.indexOf('#') + 1);
        if (fragment.isNotEmpty) {
          expect(formatError.toString(), isNot(contains(fragment)));
        }

        final fault = faultFor(reason);
        if (fault != null) {
          final (code, detail) = fault.value;
          expect(formatError.message, allOf(contains(code), contains(detail)));
        }
      });
    }
  });

  group('application key and port rules', () {
    test('an uppercase application key throws HB-APPKEY-INVALID with detail uppercase', () {
      final error = thrownBy('https://holebridge.app/k#7KQM4X9TR.${'F' * 64}');
      expect(error, isA<FormatException>());
      expect(
        (error as FormatException).message,
        allOf(contains('HB-APPKEY-INVALID'), contains('uppercase')),
      );
    });

    test('ports 1 and 65535 are valid handoff ports', () {
      for (final port in [1, 65535]) {
        expect(
          parseAppLink('https://holebridge.app/h#1.${'1' * 64}.${'2' * 32}.$port.192.0.2.10'),
          isA<HandoffLink>().having((link) => link.port, 'port', port),
        );
      }
    });

    test('port 65536 throws FormatException', () {
      expect(
        () => parseAppLink('https://holebridge.app/h#1.${'1' * 64}.${'2' * 32}.65536.192.0.2.10'),
        throwsFormatException,
      );
    });

    test('a key link on another host parses', () {
      expect(
        parseAppLink(
          'https://holebridge.example/k#7KQM4X9TR.000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f',
        ),
        isA<KeyLink>().having((link) => link.key, 'key', '7KQM4X9TR'),
      );
    });
  });

  group('buildKeyLink returns each key link in links.json', () {
    for (var i = 0; i < keyLinks.length; i++) {
      final entry = keyLinks[i];
      test('keyLinks[$i] is built from its key and application key', () {
        expect(
          buildKeyLink(base, entry['key'] as String, bytesOf(entry['appKey'] as String)),
          entry['link'] as String,
        );
      });
    }
  });

  group('edge cases the vectors do not cover', () {
    const appKey = '000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f';
    final handoffTail = '.${'2' * 32}.9000.192.0.2.10';
    final handoffHead = '1.${'1' * 64}';

    void expectKeyFault(String link, String code, String detail) {
      expect(
        thrownBy(link),
        isA<FormatException>().having(
          (error) => error.message,
          'message',
          allOf(contains(code), contains(detail)),
        ),
      );
    }

    test('a key link over http is a key-link fault for its link', () {
      expectKeyFault('http://holebridge.app/k#7KQM4X9TR.$appKey', 'HB-KEY-INVALID', 'link');
    });

    test('text that is not a URL is a key-link fault for its link', () {
      expectKeyFault('not a link', 'HB-KEY-INVALID', 'link');
    });

    test('a key of 8 symbols is a key-link fault with reason length', () {
      expectKeyFault('https://holebridge.app/k#7KQM4X9T.$appKey', 'HB-KEY-INVALID', 'length');
    });

    test('a key with U is a key-link fault with reason character', () {
      expectKeyFault('https://holebridge.app/k#7KQM4X9TU.$appKey', 'HB-KEY-INVALID', 'character');
    });

    test('a handoff link over http throws FormatException', () {
      expect(
        () => parseAppLink('http://holebridge.app/h#$handoffHead$handoffTail'),
        throwsFormatException,
      );
    });

    test('handoff fields that are not in the documented form throw FormatException', () {
      // Uppercase public key, leading zero in an octet, octet above 255, empty address item.
      final bad = [
        'https://holebridge.app/h#1.${'A' * 64}.${'2' * 32}.9000.192.0.2.10',
        'https://holebridge.app/h#1.${'1' * 64}.${'2' * 32}.9000.192.0.2.010',
        'https://holebridge.app/h#1.${'1' * 64}.${'2' * 32}.9000.192.0.2.256',
        'https://holebridge.app/h#1.${'1' * 64}.${'2' * 32}.9000.192.0.2.10,',
      ];
      for (var i = 0; i < bad.length; i++) {
        // The reason names the index only, so a failure never prints the link.
        expect(() => parseAppLink(bad[i]), throwsFormatException, reason: 'bad[$i]');
      }
    });

    test('buildKeyLink rejects an application key that is not 32 bytes', () {
      expect(() => buildKeyLink(base, '7KQM4X9TR', Uint8List(31)), throwsArgumentError);
    });
  });
}
