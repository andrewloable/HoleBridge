import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/diagnostics/redact.dart';

void main() {
  group('IPv6 addresses', () {
    const addresses = [
      '::',
      '::1',
      '::ffff',
      '::a:b',
      '::ffff:c0a8:101',
      '::1:2:3:4:5:6:7',
      '1::',
      '1::2',
      '1::3:4:5:6:7:8',
      'd699::7dcc:de73:bd68:0c5b:f612:0',
      '0ed8::0:21ca:964a:8737:7bf3:d8dd',
      'fe80::1',
      '2001:DB8::1',
      '2001:db8:85a3:0:0:8a2e:370:7334',
      '2001:0db8:85a3:0000:0000:8a2e:0370:7334',
    ];
    for (final address in addresses) {
      test('redacts $address', () {
        expect(redact('peer $address ok', const []), 'peer [redacted] ok');
      });
    }
  });

  test('text that is not an address or a key stays', () {
    const text = 'at 12:30:45 on app 2.7.5, HB-LOOKUP-TIMEOUT';
    expect(redact(text, const []), text);
  });

  group('a secret that overlaps itself', () {
    test('nana in nana nanana', () {
      expect(redact('nana nanana', const ['nana']), '[redacted] [redacted]');
    });

    test('ababab in xx abababab yy', () {
      expect(
        redact('xx abababab yy', const ['ababab']),
        'xx [redacted] yy',
      );
    });

    test('garage-garage in svc: garage-garage-garage', () {
      expect(
        redact('svc: garage-garage-garage', const ['garage-garage']),
        'svc: [redacted]',
      );
    });

    test('KK in KKKKK leaves no K', () {
      expect(redact('KKKKK', const ['KK']), '[redacted]');
    });
  });

  group('speed', () {
    test('200,000 characters with no secrets finish under 2 seconds', () {
      final text = 'abc-' * 50000;
      final watch = Stopwatch()..start();
      redact(text, const []);
      watch.stop();
      expect(watch.elapsed, lessThan(const Duration(seconds: 2)));
    });

    test('200,000 characters with 50 secrets finish under 2 seconds', () {
      final text = 'x' * 200000;
      final secrets = [for (var i = 0; i < 50; i++) 'service-name-$i'];
      final watch = Stopwatch()..start();
      redact(text, secrets);
      watch.stop();
      expect(watch.elapsed, lessThan(const Duration(seconds: 2)));
    });
  });
}
