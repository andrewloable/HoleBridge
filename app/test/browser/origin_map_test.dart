import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/browser/origin_map.dart';

/// A Jellyfin service with port hint 8096, bound to local port 54321, on the VPN name the docs give
/// for it. Its kind is http unless a test says otherwise.
ServiceOrigin _jellyfin({
  String kind = 'http',
  List<String> origins = const [],
  int? localPort = 54321,
}) => ServiceOrigin(
  name: 'jellyfin',
  kind: kind,
  portHint: 8096,
  origins: origins,
  localPort: localPort,
  vpnName: 'jellyfin.living-room.internal',
);

ServiceOrigin _svc(
  String name,
  int hint,
  int? local, {
  String kind = 'http',
  List<String> origins = const [],
}) => ServiceOrigin(
  name: name,
  kind: kind,
  portHint: hint,
  origins: origins,
  localPort: local,
  vpnName: '$name.living-room.internal',
);

OriginContext _ctx(List<ServiceOrigin> services, {bool vpn = false}) =>
    OriginContext(
      lanAddresses: const ['192.168.1.10'],
      services: services,
      vpnMode: vpn,
    );

Uri? _map(String url, OriginContext ctx) => mapNavigation(Uri.parse(url), ctx);

void main() {
  group('mapNavigation', () {
    test('maps a host LAN address at a service port hint to that service local port', () {
      final ctx = OriginContext(
        lanAddresses: const ['192.168.1.10'],
        services: [_jellyfin()],
        vpnMode: false,
      );

      final mapped = mapNavigation(
        Uri.parse('http://192.168.1.10:8096/web/'),
        ctx,
      );

      expect(mapped, Uri.parse('http://127.0.0.1:54321/web/'));
    });

    test('maps a listed origin of a service onto the tunnel, on the scheme of its kind', () {
      final ctx = OriginContext(
        lanAddresses: const [],
        services: [
          _jellyfin(kind: 'https', origins: const ['https://jellyfin.example']),
        ],
        vpnMode: false,
      );

      final mapped = mapNavigation(
        Uri.parse('https://jellyfin.example/x'),
        ctx,
      );

      expect(mapped, Uri.parse('https://127.0.0.1:54321/x'));
    });

    test(
      'the mapped scheme follows the service kind, not the scheme of the link',
      () {
        final ctx = OriginContext(
          lanAddresses: const [],
          services: [
            _jellyfin(
              kind: 'http',
              origins: const ['https://jellyfin.example'],
            ),
          ],
          vpnMode: false,
        );

        final mapped = mapNavigation(
          Uri.parse('https://jellyfin.example/x'),
          ctx,
        );

        expect(mapped, Uri.parse('http://127.0.0.1:54321/x'));
      },
    );

    test('leaves an unrelated origin unmapped (returns null)', () {
      final ctx = OriginContext(
        lanAddresses: const ['192.168.1.10'],
        services: [
          _jellyfin(kind: 'https', origins: const ['https://jellyfin.example']),
        ],
        vpnMode: false,
      );

      expect(mapNavigation(Uri.parse('https://example.com'), ctx), isNull);
    });

    test(
      'with VPN mode on, maps to the service VPN name, keeping path and query',
      () {
        final ctx = OriginContext(
          lanAddresses: const ['192.168.1.10'],
          services: [_jellyfin()],
          vpnMode: true,
        );

        final mapped = mapNavigation(
          Uri.parse('http://192.168.1.10:8096/web/?a=1'),
          ctx,
        );

        expect(mapped, isNotNull);
        expect(mapped!.host, 'jellyfin.living-room.internal');
        expect(mapped.path, '/web/');
        expect(mapped.query, 'a=1');
      },
    );

    test('leaves a host LAN address at a port no service hints at unmapped (returns null)', () {
      final ctx = OriginContext(
        lanAddresses: const ['192.168.1.10'],
        services: [_jellyfin()],
        vpnMode: false,
      );

      expect(
        mapNavigation(Uri.parse('http://192.168.1.10:9999/web/'), ctx),
        isNull,
      );
    });

    test('leaves an address that is not a host LAN address unmapped, even at a port hint', () {
      final ctx = OriginContext(
        lanAddresses: const ['192.168.1.10'],
        services: [_jellyfin()],
        vpnMode: false,
      );

      expect(
        mapNavigation(Uri.parse('http://192.168.1.99:8096/web/'), ctx),
        isNull,
      );
    });
  });

  group('mapNavigation trust boundary', () {
    final two = _ctx([
      _svc(
        'jellyfin',
        8096,
        54321,
        origins: const ['https://jellyfin.example'],
      ),
      _svc('grafana', 3000, 54322, origins: const ['https://grafana.example']),
    ]);

    test('maps each link to its own service, never to another', () {
      expect(
        _map('http://192.168.1.10:3000/d/1', two),
        Uri.parse('http://127.0.0.1:54322/d/1'),
      );
      expect(
        _map('http://192.168.1.10:8096/web/', two),
        Uri.parse('http://127.0.0.1:54321/web/'),
      );
      expect(
        _map('https://grafana.example/d/1', two),
        Uri.parse('http://127.0.0.1:54322/d/1'),
      );
      expect(
        _map('https://jellyfin.example/x', two),
        Uri.parse('http://127.0.0.1:54321/x'),
      );
    });

    test('with VPN mode on, each link goes to its own service VPN name', () {
      final vpn = _ctx([
        _svc(
          'jellyfin',
          8096,
          54321,
          origins: const ['https://jellyfin.example'],
        ),
        _svc(
          'grafana',
          3000,
          54322,
          origins: const ['https://grafana.example'],
        ),
      ], vpn: true);
      expect(
        _map('http://192.168.1.10:3000/d/1', vpn)!.host,
        'grafana.living-room.internal',
      );
      expect(
        _map('https://jellyfin.example/x', vpn)!.host,
        'jellyfin.living-room.internal',
      );
    });

    test('a listed origin matches on scheme, host and port', () {
      final c = _ctx([
        _svc('a', 8096, 54321, origins: const ['http://jellyfin.example:8096']),
        _svc('b', 3000, 54322, origins: const ['https://grafana.example']),
      ]);
      expect(
        _map('http://jellyfin.example:8096/web', c),
        Uri.parse('http://127.0.0.1:54321/web'),
      );
      expect(
        _map('https://grafana.example:443/x', c),
        Uri.parse('http://127.0.0.1:54322/x'),
      );
      expect(_map('http://jellyfin.example:8097/web', c), isNull);
      expect(_map('http://jellyfin.example/web', c), isNull);
      expect(_map('https://jellyfin.example:8096/web', c), isNull);
      expect(_map('http://grafana.example/x', c), isNull);
      expect(_map('https://grafana.example:8443/x', c), isNull);
    });

    test('a look-alike host is not the host', () {
      for (final url in [
        'https://jellyfin.example.evil.com/x',
        'https://notjellyfin.example/x',
        'https://jellyfin.example@evil.com/x',
        'http://192.168.1.10:8096@evil.com/',
        'http://192.168.1.100:8096/',
        'http://192.168.1.10.evil.com:8096/',
      ]) {
        expect(_map(url, two), isNull, reason: url);
      }
    });

    test('the loopback address is never a service real address', () {
      for (final url in [
        'http://127.0.0.1:8096/web/',
        'http://127.0.0.1:54321/web/',
        'http://localhost:8096/web/',
      ]) {
        expect(_map(url, two), isNull, reason: url);
      }
    });

    test('a service that is not known to be a web service never maps', () {
      final c = _ctx([
        _svc(
          'ssh',
          22,
          54323,
          kind: 'tcp',
          origins: const ['http://ssh.example:22'],
        ),
        _svc('dns', 53, 54324, kind: 'udp'),
        _svc('box', 8000, 54325, kind: 'unknown'),
      ]);
      expect(_map('http://192.168.1.10:22/', c), isNull);
      expect(_map('http://ssh.example:22/', c), isNull);
      expect(_map('http://192.168.1.10:53/', c), isNull);
      expect(_map('http://192.168.1.10:8000/', c), isNull);
    });

    test(
      'a link that is not http or https is left alone, and does not throw',
      () {
        for (final url in [
          'about:blank',
          'mailto:a@b.example',
          'javascript:void(0)',
          'data:text/html,hi',
          'file:///etc/hosts',
          'ftp://192.168.1.10:8096/',
          'ws://192.168.1.10:8096/socket',
          '/web/',
        ]) {
          expect(_map(url, two), isNull, reason: url);
        }
      },
    );

    test('with no local port bound and VPN mode off, nothing maps', () {
      final c = _ctx([_svc('jellyfin', 8096, null)]);
      expect(_map('http://192.168.1.10:8096/web/', c), isNull);
    });

    test('with VPN mode on, a service with no local port still maps to its VPN name', () {
      final c = _ctx([_svc('jellyfin', 8096, null)], vpn: true);
      expect(
        _map('http://192.168.1.10:8096/web/', c)!.host,
        'jellyfin.living-room.internal',
      );
    });
  });

  group('mapNavigation loopback and VPN name guards', () {
    test(
      'a loopback spelling is never mapped, even when the host lists it',
      () {
        for (final host in [
          '[::ffff:127.0.0.1]',
          '[::ffff:7f00:1]',
          '[::ffff:0.0.0.0]',
          'localhost.',
          '127.1',
          '2130706433',
          '0x7f.1',
          '0177.0.0.1',
          '[::1]',
          '[::]',
          '0.0.0.0',
        ]) {
          final bare = host.replaceAll('[', '').replaceAll(']', '');
          final byLan = OriginContext(
            lanAddresses: [bare],
            services: [_jellyfin()],
            vpnMode: false,
          );
          final byOrigin = OriginContext(
            lanAddresses: const [],
            services: [
              _jellyfin(origins: ['http://$host:8096']),
            ],
            vpnMode: false,
          );
          final url = Uri.parse('http://$host:8096/web');
          expect(
            mapNavigation(url, byLan),
            isNull,
            reason: 'LAN address $host',
          );
          expect(
            mapNavigation(url, byOrigin),
            isNull,
            reason: 'listed origin $host',
          );
        }
      },
    );

    test('a vpn name that is not a host name never maps and never throws', () {
      for (final vpnName in [
        '',
        'a/b.x.internal',
        'a:b.x.internal',
        'a@b.x.internal',
        'a?b.x.internal',
        'a#b.x.internal',
        'a b.x.internal',
        '127.0.0.1',
        '0.0.0.0',
        'LOCALHOST',
      ]) {
        final ctx = OriginContext(
          lanAddresses: const ['192.168.1.10'],
          services: [_withVpnName(vpnName)],
          vpnMode: true,
        );
        final url = Uri.parse('http://192.168.1.10:8096/web/');
        expect(() => mapNavigation(url, ctx), returnsNormally, reason: vpnName);
        expect(mapNavigation(url, ctx), isNull, reason: 'vpn name "$vpnName"');
      }
    });

    test('a normal LAN address and a normal vpn name still map', () {
      for (final vpnName in [
        'jellyfin.living-room.internal',
        'jellyfin.my-phone.internal',
      ]) {
        final ctx = OriginContext(
          lanAddresses: const ['192.168.1.10'],
          services: [_withVpnName(vpnName)],
          vpnMode: true,
        );
        expect(
          mapNavigation(Uri.parse('http://192.168.1.10:8096/web/'), ctx)?.host,
          vpnName,
        );
      }
    });

    test('a vpn name from a service name that ends with a dash still maps', () {
      for (final vpnName in [
        'web-.living-room.internal',
        'a--b.living-room.internal',
        '8080.living-room.internal',
      ]) {
        final ctx = OriginContext(
          lanAddresses: const ['192.168.1.10'],
          services: [_withVpnName(vpnName)],
          vpnMode: true,
        );
        expect(
          mapNavigation(Uri.parse('http://192.168.1.10:8096/web/'), ctx)?.host,
          vpnName,
          reason: vpnName,
        );
      }
    });

    test('a vpn name that starts with a dash never maps', () {
      final ctx = OriginContext(
        lanAddresses: const ['192.168.1.10'],
        services: [_withVpnName('-a.living-room.internal')],
        vpnMode: true,
      );
      expect(
        mapNavigation(Uri.parse('http://192.168.1.10:8096/web/'), ctx),
        isNull,
      );
    });
  });
}

/// A Jellyfin-like http service on port hint 8096, local port 54321, with [vpnName] as its VPN name.
ServiceOrigin _withVpnName(String vpnName) => ServiceOrigin(
  name: 'jellyfin',
  kind: 'http',
  portHint: 8096,
  origins: const [],
  localPort: 54321,
  vpnName: vpnName,
);
