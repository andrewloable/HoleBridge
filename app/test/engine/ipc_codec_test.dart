import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:holebridge/src/engine/ipc_codec.dart';

import '../helpers/vectors.dart';

/// Every frame example of spec/vectors/ipc.json, each with its name, type, id, value and hex.
final _vectors = (loadVector('ipc.json')['vectors'] as List<dynamic>).cast<Map<String, dynamic>>();

Uint8List _bytes(String hex) => Uint8List.fromList([
  for (var i = 0; i < hex.length; i += 2) int.parse(hex.substring(i, i + 2), radix: 16),
]);

String _hex(Uint8List bytes) => bytes.map((b) => b.toRadixString(16).padLeft(2, '0')).join();

List<String> _strings(Object? json) => (json as List<dynamic>).cast<String>();

List<PortBinding> _ports(Object? json) => [
  for (final p in json as List<dynamic>)
    PortBinding(service: (p as Map<String, dynamic>)['service'] as String, port: p['port'] as int),
];

List<Map<String, dynamic>> _portsJson(List<PortBinding> ports) => [
  for (final p in ports) {'service': p.service, 'port': p.port},
];

List<ServiceEntry> _serviceEntries(Object? json) => [
  for (final s in json as List<dynamic>)
    ServiceEntry(name: (s as Map<String, dynamic>)['name'] as String, kind: s['kind'] as int),
];

List<Map<String, dynamic>> _serviceEntriesJson(List<ServiceEntry> list) => [
  for (final s in list) {'name': s.name, 'kind': s.kind},
];

LanAddresses _lan(Object? json) {
  final lan = json as Map<String, dynamic>;
  return LanAddresses(addresses: _strings(lan['addresses']), port: lan['port'] as int);
}

NatInfo _nat(Object? json) {
  final nat = json as Map<String, dynamic>;
  return NatInfo(
    host: nat['host'] as String,
    port: nat['port'] as int,
    firewalled: nat['firewalled'] as bool,
    randomized: nat['randomized'] as bool,
  );
}

VpnService _vpnService(Map<String, dynamic> json) => VpnService(
  host: json['host'] as String,
  service: json['service'] as String,
  address: json['address'] as String,
  port: json['port'] as int,
);

VpnAddress _vpnAddress(Map<String, dynamic> json) => VpnAddress(
  host: json['host'] as String,
  service: json['service'] as String,
  address: json['address'] as String,
  name: json['name'] as String,
);

/// The message that vector [v] describes, built from its type, id and value.
IpcMessage _fromVector(Map<String, dynamic> v) {
  final id = v['id'] as int;
  final value = v['value'] as Map<String, dynamic>;
  switch (v['type'] as int) {
    case 0:
      return IpcReply(
        id: id,
        ok: value['ok'] as bool,
        code: value['code'] as String,
        detail: value['detail'] as String,
        route: value['route'] as String,
        services: _serviceEntries(value['services']),
        ports: _ports(value['ports']),
      );
    case 1:
      return ConnectRequest(
        id: id,
        host: value['host'] as String,
        key: value['key'] as String,
        appKey: _bytes(value['appKey'] as String),
        lan: _lan(value['lan']),
        ports: _ports(value['ports']),
        bind: value['bind'] as String,
      );
    case 2:
      return CloseRequest(id: id, host: value['host'] as String);
    case 3:
      return StatusRequest(id: id, host: value['host'] as String);
    case 4:
      return RelayRequest(
        id: id,
        key: value['key'] as String,
        appKey: _bytes(value['appKey'] as String),
      );
    case 5:
      return HandoffListenRequest(id: id);
    case 6:
      return HandoffCancelRequest(id: id);
    case 7:
      return HandoffSendRequest(
        id: id,
        link: value['link'] as String,
        name: value['name'] as String,
        key: value['key'] as String,
        appKey: _bytes(value['appKey'] as String),
      );
    case 8:
      return VpnStartRequest(
        id: id,
        services: [
          for (final s in value['services'] as List<dynamic>)
            _vpnService(s as Map<String, dynamic>),
        ],
        dnsUpstream: _strings(value['dnsUpstream']),
      );
    case 9:
      return VpnStopRequest(id: id);
    case 100:
      return RouteEvent(host: value['host'] as String, route: value['route'] as String);
    case 101:
      return SessionEvent(host: value['host'] as String, up: value['up'] as bool);
    case 102:
      return StatusEvent(
        host: value['host'] as String,
        route: value['route'] as String,
        sessions: value['sessions'] as int,
        streams: value['streams'] as int,
        flows: value['flows'] as int,
        bytesIn: value['bytesIn'] as int,
        bytesOut: value['bytesOut'] as int,
        nat: _nat(value['nat']),
      );
    case 103:
      return VpnEvent(
        port: value['port'] as int,
        addresses: [
          for (final a in value['addresses'] as List<dynamic>)
            _vpnAddress(a as Map<String, dynamic>),
        ],
      );
    case 104:
      return ServicesEvent(
        host: value['host'] as String,
        list: _serviceEntries(value['list']),
        ports: _ports(value['ports']),
      );
    case 105:
      return RejectEvent(
        host: value['host'] as String,
        service: value['service'] as String,
        code: value['code'] as String,
        reason: value['reason'] as String,
      );
    case 106:
      return ErrorEvent(code: value['code'] as String, detail: value['detail'] as String);
    case 107:
      return HandoffCodeEvent(link: value['link'] as String);
    case 108:
      return HandoffReceivedEvent(
        name: value['name'] as String,
        key: value['key'] as String,
        appKey: _bytes(value['appKey'] as String),
      );
    case 109:
      return LanEvent(
        host: value['host'] as String,
        addresses: _strings(value['addresses']),
        port: value['port'] as int,
      );
  }
  throw ArgumentError('no message for type ${v['type']} in vector ${v['name']}');
}

Map<String, dynamic> _vpnServiceJson(VpnService s) => {
  'host': s.host,
  'service': s.service,
  'address': s.address,
  'port': s.port,
};

Map<String, dynamic> _vpnAddressJson(VpnAddress a) => {
  'host': a.host,
  'service': a.service,
  'address': a.address,
  'name': a.name,
};

/// The body of [m] written the way the vectors write it: the fields as JSON, fixed32 as hex.
Map<String, dynamic> _valueOf(IpcMessage m) {
  switch (m) {
    case IpcReply r:
      return {
        'ok': r.ok,
        'code': r.code,
        'detail': r.detail,
        'route': r.route,
        'services': _serviceEntriesJson(r.services),
        'ports': _portsJson(r.ports),
      };
    case ConnectRequest r:
      return {
        'host': r.host,
        'key': r.key,
        'appKey': _hex(r.appKey),
        'lan': {'addresses': r.lan.addresses, 'port': r.lan.port},
        'ports': _portsJson(r.ports),
        'bind': r.bind,
      };
    case CloseRequest r:
      return {'host': r.host};
    case StatusRequest r:
      return {'host': r.host};
    case RelayRequest r:
      return {'key': r.key, 'appKey': _hex(r.appKey)};
    case HandoffListenRequest():
    case HandoffCancelRequest():
    case VpnStopRequest():
      return {};
    case HandoffSendRequest r:
      return {'link': r.link, 'name': r.name, 'key': r.key, 'appKey': _hex(r.appKey)};
    case VpnStartRequest r:
      return {
        'services': [for (final s in r.services) _vpnServiceJson(s)],
        'dnsUpstream': r.dnsUpstream,
      };
    case RouteEvent r:
      return {'host': r.host, 'route': r.route};
    case SessionEvent r:
      return {'host': r.host, 'up': r.up};
    case StatusEvent r:
      return {
        'host': r.host,
        'route': r.route,
        'sessions': r.sessions,
        'streams': r.streams,
        'flows': r.flows,
        'bytesIn': r.bytesIn,
        'bytesOut': r.bytesOut,
        'nat': {
          'host': r.nat.host,
          'port': r.nat.port,
          'firewalled': r.nat.firewalled,
          'randomized': r.nat.randomized,
        },
      };
    case VpnEvent r:
      return {
        'port': r.port,
        'addresses': [for (final a in r.addresses) _vpnAddressJson(a)],
      };
    case ServicesEvent r:
      return {'host': r.host, 'list': _serviceEntriesJson(r.list), 'ports': _portsJson(r.ports)};
    case RejectEvent r:
      return {'host': r.host, 'service': r.service, 'code': r.code, 'reason': r.reason};
    case ErrorEvent r:
      return {'code': r.code, 'detail': r.detail};
    case HandoffCodeEvent r:
      return {'link': r.link};
    case HandoffReceivedEvent r:
      return {'name': r.name, 'key': r.key, 'appKey': _hex(r.appKey)};
    case LanEvent r:
      return {'host': r.host, 'addresses': r.addresses, 'port': r.port};
    default:
      throw ArgumentError('no vector shape for ${m.runtimeType}');
  }
}

void main() {
  for (final v in _vectors) {
    final label = '${v['name']} (type ${v['type']})';

    test('encode equals hex for $label', () {
      expect(_hex(encode(_fromVector(v))), v['hex']);
    });

    test('decode of $label equals its value', () {
      final decoded = decode(_bytes(v['hex'] as String));
      expect(decoded.type, v['type']);
      expect(decoded.id, v['id']);
      expect(_valueOf(decoded), v['value']);
    });
  }

  test('decode throws IpcDesync on a truncated frame', () {
    // route 1 from the vectors, without its last byte: the host string is cut short.
    final truncated = _bytes('64000b6c6976696e672d726f6f6d036c61');
    expect(() => decode(truncated), throwsA(isA<IpcDesync>()));
  });

  test('decode throws IpcDesync when bytes are left over after the body', () {
    // reply 1 from the vectors, with one extra byte at the end.
    final extra = _bytes('000101000000000000');
    expect(() => decode(extra), throwsA(isA<IpcDesync>()));
  });

  test('decode throws IpcDesync on a type no message has', () {
    // Stray stdout text: its first byte, 'H' (0x48), is no message type.
    expect(() => decode(Uint8List.fromList('Hello'.codeUnits)), throwsA(isA<IpcDesync>()));
    // 10 is the first type number no message uses yet.
    expect(() => decode(_bytes('0a0001')), throwsA(isA<IpcDesync>()));
  });

  test('decode throws IpcDesync when the id does not fit the message kind', () {
    // A request (type 3) with id 0: requests need a nonzero id. The body is complete.
    expect(() => decode(_bytes('030000')), throwsA(isA<IpcDesync>()));
    // A reply (type 0) with id 0: replies need the id of their request.
    expect(() => decode(_bytes('0000000000000000')), throwsA(isA<IpcDesync>()));
    // A route event (type 100) with id 5: events have id 0.
    expect(() => decode(_bytes('64050000')), throwsA(isA<IpcDesync>()));
  });

  test('decode throws IpcDesync on a bool byte above 1', () {
    // reply 1 from the vectors, with ok set to 2.
    expect(() => decode(_bytes('0001020000000000')), throwsA(isA<IpcDesync>()));
    // session 1 from the vectors, with up set to 2.
    expect(() => decode(_bytes('65000b6c6976696e672d726f6f6d02')), throwsA(isA<IpcDesync>()));
  });

  test('decode keeps a leading byte order mark in a string, as the engine does', () {
    // error event (type 106, id 0): code is U+FEFF then "bom"; detail is empty.
    final m = decode(_bytes('6a0006efbbbf626f6d00')) as ErrorEvent;
    expect(m.code.codeUnits, [0xfeff, 0x62, 0x6f, 0x6d]);
    expect(_hex(encode(m)), '6a0006efbbbf626f6d00');
    // a code that is only the BOM
    expect((decode(_bytes('6a0003efbbbf00')) as ErrorEvent).code.codeUnits, [0xfeff]);
  });
}
