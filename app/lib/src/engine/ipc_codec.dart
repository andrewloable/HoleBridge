import 'dart:convert';
import 'dart:typed_data';

// The frames of the app engine IPC, as spec/ipc.md defines them. Each frame is a type (the message
// number), an id and a body. This file holds the message classes and the codec; the client that
// sends and pairs them is engine_client.dart.

/// A frame that does not decode exactly (HB-IPC-DESYNC). The client treats it as fatal.
class IpcDesync implements Exception {
  const IpcDesync();
}

/// One frame of the app engine IPC.
abstract class IpcMessage {
  const IpcMessage();

  /// The message number from the tables in spec/ipc.md.
  int get type;

  /// Pairs a request with its reply. Dart picks request ids, never 0. Events have id 0.
  int get id;
}

/// A request from Dart to the engine (types 1 to 99).
abstract class IpcRequest extends IpcMessage {
  const IpcRequest();

  /// A copy of this request that carries [id]. The client uses it to give each request its id.
  IpcRequest withId(int id);
}

/// An event from the engine (types 100 and up). Events have id 0.
abstract class IpcEvent extends IpcMessage {
  const IpcEvent();

  @override
  int get id => 0;
}

/// The reply to a request (type 0), with the request's id. Only the connect reply fills [route],
/// [services] and [ports].
class IpcReply extends IpcMessage {
  const IpcReply({
    this.id = 0,
    required this.ok,
    this.code = '',
    this.detail = '',
    this.route = '',
    this.services = const [],
    this.ports = const [],
  });

  @override
  final int id;

  @override
  int get type => 0;

  final bool ok;
  final String code;
  final String detail;
  final String route;
  final List<ServiceEntry> services;
  final List<PortBinding> ports;
}

/// A service entry (the `services` struct of connect and the `list` of the services event): the
/// service's name, then its kind in the handshake's numbering (0 unknown, 1 https, 2 http, 3 tcp,
/// 4 udp; spec/ipc.md, Encoding).
class ServiceEntry {
  const ServiceEntry({required this.name, required this.kind});

  final String name;
  final int kind;
}

/// A service and the local port bound for it (the `ports` struct).
class PortBinding {
  const PortBinding({required this.service, required this.port});

  final String service;
  final int port;
}

/// The `lan` struct of connect: the host's LAN addresses and port. Empty when none is known.
class LanAddresses {
  const LanAddresses({required this.addresses, required this.port});

  final List<String> addresses;
  final int port;
}

/// The `nat` struct of the status event: the engine's own NAT view.
class NatInfo {
  const NatInfo({
    required this.host,
    required this.port,
    required this.firewalled,
    required this.randomized,
  });

  final String host;
  final int port;
  final bool firewalled;
  final bool randomized;
}

/// One entry of the `services` array of vpn.start.
class VpnService {
  const VpnService({
    required this.host,
    required this.service,
    required this.address,
    required this.port,
  });

  final String host;
  final String service;
  final String address;
  final int port;
}

/// One entry of the `addresses` array of the vpn event.
class VpnAddress {
  const VpnAddress({
    required this.host,
    required this.service,
    required this.address,
    required this.name,
  });

  final String host;
  final String service;
  final String address;
  final String name;
}

// Requests (Dart to engine).

/// connect (type 1).
class ConnectRequest extends IpcRequest {
  const ConnectRequest({
    this.id = 0,
    required this.host,
    required this.key,
    required this.appKey,
    required this.lan,
    this.ports = const [],
    required this.bind,
  });

  @override
  final int id;

  @override
  int get type => 1;

  final String host;
  final String key;

  /// The application key: 32 bytes.
  final Uint8List appKey;
  final LanAddresses lan;
  final List<PortBinding> ports;
  final String bind;

  @override
  IpcRequest withId(int id) => ConnectRequest(
    id: id,
    host: host,
    key: key,
    appKey: appKey,
    lan: lan,
    ports: ports,
    bind: bind,
  );
}

/// close (type 2).
class CloseRequest extends IpcRequest {
  const CloseRequest({this.id = 0, required this.host});

  @override
  final int id;

  @override
  int get type => 2;

  final String host;

  @override
  IpcRequest withId(int id) => CloseRequest(id: id, host: host);
}

/// status (type 3).
class StatusRequest extends IpcRequest {
  const StatusRequest({this.id = 0, required this.host});

  @override
  final int id;

  @override
  int get type => 3;

  final String host;

  @override
  IpcRequest withId(int id) => StatusRequest(id: id, host: host);
}

/// relay (type 4). An empty [key] means no relay.
class RelayRequest extends IpcRequest {
  const RelayRequest({this.id = 0, required this.key, required this.appKey});

  @override
  final int id;

  @override
  int get type => 4;

  final String key;

  /// The application key: 32 bytes.
  final Uint8List appKey;

  @override
  IpcRequest withId(int id) => RelayRequest(id: id, key: key, appKey: appKey);
}

/// handoff.listen (type 5).
class HandoffListenRequest extends IpcRequest {
  const HandoffListenRequest({this.id = 0});

  @override
  final int id;

  @override
  int get type => 5;

  @override
  IpcRequest withId(int id) => HandoffListenRequest(id: id);
}

/// handoff.cancel (type 6).
class HandoffCancelRequest extends IpcRequest {
  const HandoffCancelRequest({this.id = 0});

  @override
  final int id;

  @override
  int get type => 6;

  @override
  IpcRequest withId(int id) => HandoffCancelRequest(id: id);
}

/// handoff.send (type 7).
class HandoffSendRequest extends IpcRequest {
  const HandoffSendRequest({
    this.id = 0,
    required this.link,
    required this.name,
    required this.key,
    required this.appKey,
  });

  @override
  final int id;

  @override
  int get type => 7;

  final String link;
  final String name;
  final String key;

  /// The application key: 32 bytes.
  final Uint8List appKey;

  @override
  IpcRequest withId(int id) =>
      HandoffSendRequest(id: id, link: link, name: name, key: key, appKey: appKey);
}

/// vpn.start (type 8).
class VpnStartRequest extends IpcRequest {
  const VpnStartRequest({this.id = 0, required this.services, required this.dnsUpstream});

  @override
  final int id;

  @override
  int get type => 8;

  final List<VpnService> services;
  final List<String> dnsUpstream;

  @override
  IpcRequest withId(int id) =>
      VpnStartRequest(id: id, services: services, dnsUpstream: dnsUpstream);
}

/// vpn.stop (type 9).
class VpnStopRequest extends IpcRequest {
  const VpnStopRequest({this.id = 0});

  @override
  final int id;

  @override
  int get type => 9;

  @override
  IpcRequest withId(int id) => VpnStopRequest(id: id);
}

// Events (engine to Dart).

/// route (type 100).
class RouteEvent extends IpcEvent {
  const RouteEvent({required this.host, required this.route});

  @override
  int get type => 100;

  final String host;

  /// lan, direct, relay, looking or unreachable.
  final String route;
}

/// session (type 101).
class SessionEvent extends IpcEvent {
  const SessionEvent({required this.host, required this.up});

  @override
  int get type => 101;

  final String host;
  final bool up;
}

/// status (type 102), sent after a status request.
class StatusEvent extends IpcEvent {
  const StatusEvent({
    required this.host,
    required this.route,
    required this.sessions,
    required this.streams,
    required this.flows,
    required this.bytesIn,
    required this.bytesOut,
    required this.nat,
  });

  @override
  int get type => 102;

  final String host;

  /// As in RouteEvent, or "" when no session is up and no search runs.
  final String route;
  final int sessions;
  final int streams;
  final int flows;
  final int bytesIn;
  final int bytesOut;
  final NatInfo nat;
}

/// vpn (type 103), sent after vpn.start.
class VpnEvent extends IpcEvent {
  const VpnEvent({required this.port, required this.addresses});

  @override
  int get type => 103;

  /// The TCP port on 127.0.0.1 of the engine's SOCKS5 front. The network stack connects to it
  /// (spec/ipc.md point 7).
  final int port;
  final List<VpnAddress> addresses;
}

/// services (type 104).
class ServicesEvent extends IpcEvent {
  const ServicesEvent({required this.host, required this.list, required this.ports});

  @override
  int get type => 104;

  final String host;
  final List<ServiceEntry> list;
  final List<PortBinding> ports;
}

/// reject (type 105).
class RejectEvent extends IpcEvent {
  const RejectEvent({
    required this.host,
    required this.service,
    required this.code,
    required this.reason,
  });

  @override
  int get type => 105;

  final String host;
  final String service;

  /// An HB- code from spec/errors.json.
  final String code;
  final String reason;
}

/// error (type 106).
class ErrorEvent extends IpcEvent {
  const ErrorEvent({required this.code, required this.detail});

  @override
  int get type => 106;

  /// An HB- code from spec/errors.json.
  final String code;
  final String detail;
}

/// handoff.code (type 107), sent after handoff.listen.
class HandoffCodeEvent extends IpcEvent {
  const HandoffCodeEvent({required this.link});

  @override
  int get type => 107;

  final String link;
}

/// handoff.received (type 108), sent when the TV accepts a box.
class HandoffReceivedEvent extends IpcEvent {
  const HandoffReceivedEvent({required this.name, required this.key, required this.appKey});

  @override
  int get type => 108;

  final String name;
  final String key;

  /// The application key: 32 bytes.
  final Uint8List appKey;
}

/// lan (type 109), sent when a handshake carries the host's LAN addresses.
class LanEvent extends IpcEvent {
  const LanEvent({required this.host, required this.addresses, required this.port});

  @override
  int get type => 109;

  final String host;
  final List<String> addresses;
  final int port;
}

/// The largest uint the engine's compact-encoding accepts: Number.MAX_SAFE_INTEGER, 2^53 - 1.
const _maxSafeUint = 0x1fffffffffffff;

/// The most items an array may hold. compact-encoding refuses a longer array on decode.
const _maxArrayLength = 0x100000;

/// Encodes [m] as one frame: its type, its id and its body, as spec/ipc.md defines them.
///
/// Throws ArgumentError for a message the wire cannot carry: an id that does not fit its kind, a
/// number out of range, or an application key that is not 32 bytes.
Uint8List encode(IpcMessage m) {
  if ((m.type >= 100) != (m.id == 0)) {
    throw ArgumentError.value(m.id, 'id', 'does not fit the message type ${m.type}');
  }
  final w = _Writer()
    ..uint(m.type)
    ..uint(m.id);
  switch (m) {
    case IpcReply r:
      w
        ..bool_(r.ok)
        ..string(r.code)
        ..string(r.detail)
        ..string(r.route)
        ..array(r.services, w.serviceEntry)
        ..array(r.ports, w.port);
    case ConnectRequest r:
      w
        ..string(r.host)
        ..string(r.key)
        ..fixed32(r.appKey)
        ..lan(r.lan)
        ..array(r.ports, w.port)
        ..string(r.bind);
    case CloseRequest r:
      w.string(r.host);
    case StatusRequest r:
      w.string(r.host);
    case RelayRequest r:
      w
        ..string(r.key)
        ..fixed32(r.appKey);
    case HandoffListenRequest():
    case HandoffCancelRequest():
    case VpnStopRequest():
      break;
    case HandoffSendRequest r:
      w
        ..string(r.link)
        ..string(r.name)
        ..string(r.key)
        ..fixed32(r.appKey);
    case VpnStartRequest r:
      w
        ..array(r.services, w.vpnService)
        ..array(r.dnsUpstream, w.string);
    case RouteEvent r:
      w
        ..string(r.host)
        ..string(r.route);
    case SessionEvent r:
      w
        ..string(r.host)
        ..bool_(r.up);
    case StatusEvent r:
      w
        ..string(r.host)
        ..string(r.route)
        ..uint(r.sessions)
        ..uint(r.streams)
        ..uint(r.flows)
        ..uint(r.bytesIn)
        ..uint(r.bytesOut)
        ..nat(r.nat);
    case VpnEvent r:
      w
        ..uint(r.port)
        ..array(r.addresses, w.vpnAddress);
    case ServicesEvent r:
      w
        ..string(r.host)
        ..array(r.list, w.serviceEntry)
        ..array(r.ports, w.port);
    case RejectEvent r:
      w
        ..string(r.host)
        ..string(r.service)
        ..string(r.code)
        ..string(r.reason);
    case ErrorEvent r:
      w
        ..string(r.code)
        ..string(r.detail);
    case HandoffCodeEvent r:
      w.string(r.link);
    case HandoffReceivedEvent r:
      w
        ..string(r.name)
        ..string(r.key)
        ..fixed32(r.appKey);
    case LanEvent r:
      w
        ..string(r.host)
        ..array(r.addresses, w.string)
        ..uint(r.port);
    default:
      throw ArgumentError.value(m, 'm', 'is not an IPC message');
  }
  return w.take();
}

/// Decodes one whole frame into its message. Throws [IpcDesync] when [b] does not decode exactly:
/// a truncated frame, bytes left over after the body, a type no message has, an id that does not
/// fit the type, a bool byte above 1, or any other value the engine's readFrame refuses.
IpcMessage decode(Uint8List b) {
  final r = _Reader(b);
  final type = r.uint();
  final id = r.uint();
  if ((type >= 100) != (id == 0)) throw const IpcDesync();
  final message = _body(r, type, id);
  r.end();
  return message;
}

/// The body of a frame of [type], read in the order spec/ipc.md lists its fields. Dart evaluates
/// the arguments of a constructor from left to right, which is the wire order.
IpcMessage _body(_Reader r, int type, int id) => switch (type) {
  0 => IpcReply(
    id: id,
    ok: r.bool_(),
    code: r.string(),
    detail: r.string(),
    route: r.string(),
    services: r.array(() => _serviceEntry(r)),
    ports: r.array(() => _port(r)),
  ),
  1 => ConnectRequest(
    id: id,
    host: r.string(),
    key: r.string(),
    appKey: r.fixed32(),
    lan: _lan(r),
    ports: r.array(() => _port(r)),
    bind: r.string(),
  ),
  2 => CloseRequest(id: id, host: r.string()),
  3 => StatusRequest(id: id, host: r.string()),
  4 => RelayRequest(id: id, key: r.string(), appKey: r.fixed32()),
  5 => HandoffListenRequest(id: id),
  6 => HandoffCancelRequest(id: id),
  7 => HandoffSendRequest(
    id: id,
    link: r.string(),
    name: r.string(),
    key: r.string(),
    appKey: r.fixed32(),
  ),
  8 => VpnStartRequest(
    id: id,
    services: r.array(() => _vpnService(r)),
    dnsUpstream: r.array(r.string),
  ),
  9 => VpnStopRequest(id: id),
  100 => RouteEvent(host: r.string(), route: r.string()),
  101 => SessionEvent(host: r.string(), up: r.bool_()),
  102 => StatusEvent(
    host: r.string(),
    route: r.string(),
    sessions: r.uint(),
    streams: r.uint(),
    flows: r.uint(),
    bytesIn: r.uint(),
    bytesOut: r.uint(),
    nat: _nat(r),
  ),
  103 => VpnEvent(port: r.uint(), addresses: r.array(() => _vpnAddress(r))),
  104 => ServicesEvent(
    host: r.string(),
    list: r.array(() => _serviceEntry(r)),
    ports: r.array(() => _port(r)),
  ),
  105 => RejectEvent(host: r.string(), service: r.string(), code: r.string(), reason: r.string()),
  106 => ErrorEvent(code: r.string(), detail: r.string()),
  107 => HandoffCodeEvent(link: r.string()),
  108 => HandoffReceivedEvent(name: r.string(), key: r.string(), appKey: r.fixed32()),
  109 => LanEvent(host: r.string(), addresses: r.array(r.string), port: r.uint()),
  _ => throw const IpcDesync(),
};

// The structs, read in field order.

PortBinding _port(_Reader r) => PortBinding(service: r.string(), port: r.uint());

ServiceEntry _serviceEntry(_Reader r) => ServiceEntry(name: r.string(), kind: r.uint());

LanAddresses _lan(_Reader r) => LanAddresses(addresses: r.array(r.string), port: r.uint());

NatInfo _nat(_Reader r) =>
    NatInfo(host: r.string(), port: r.uint(), firewalled: r.bool_(), randomized: r.bool_());

VpnService _vpnService(_Reader r) =>
    VpnService(host: r.string(), service: r.string(), address: r.string(), port: r.uint());

VpnAddress _vpnAddress(_Reader r) =>
    VpnAddress(host: r.string(), service: r.string(), address: r.string(), name: r.string());

/// Reads the compact-encoding 3.5.2 values of one frame, refusing whatever the engine refuses.
/// Every read past the end of the frame throws IpcDesync.
class _Reader {
  _Reader(this._bytes);

  final Uint8List _bytes;
  int _at = 0;

  int get _left => _bytes.length - _at;

  int _byte() {
    if (_left < 1) throw const IpcDesync();
    return _bytes[_at++];
  }

  /// [n] bytes, little-endian, as compact-encoding reads them.
  int _le(int n) {
    if (_left < n) throw const IpcDesync();
    var v = 0;
    for (var i = 0; i < n; i++) {
      v += _bytes[_at++] << (8 * i);
    }
    return v;
  }

  /// A uint. The short forms and the wider ones are read as compact-encoding reads them, so a
  /// value that is not in its shortest form is still accepted, as the engine accepts it.
  int uint() {
    final a = _byte();
    if (a <= 0xfc) return a;
    if (a == 0xfd) return _le(2);
    if (a == 0xfe) return _le(4);
    final lo = _le(4);
    final hi = _le(4);
    if (hi > 0x1fffff) throw const IpcDesync();
    return hi * 0x100000000 + lo;
  }

  /// A bool is one byte: 0 or 1. Any other byte does not decode exactly.
  bool bool_() {
    final b = _byte();
    if (b > 1) throw const IpcDesync();
    return b == 1;
  }

  /// A length-prefixed UTF-8 string. Malformed UTF-8 is replaced, as the engine's decoder does.
  /// Dart's decoder drops a leading byte order mark (U+FEFF); this puts it back, as the engine's
  /// decoder keeps it.
  String string() {
    final n = uint();
    if (_left < n) throw const IpcDesync();
    final bom = n >= 3 && _bytes[_at] == 0xef && _bytes[_at + 1] == 0xbb && _bytes[_at + 2] == 0xbf;
    final s = utf8.decode(_bytes.sublist(_at, _at + n), allowMalformed: true);
    _at += n;
    return bom ? '\uFEFF$s' : s;
  }

  /// The 32 raw bytes of an application key, copied out of the frame.
  Uint8List fixed32() {
    if (_left < 32) throw const IpcDesync();
    final v = Uint8List.fromList(_bytes.sublist(_at, _at + 32));
    _at += 32;
    return v;
  }

  /// A count, then that many items read by [item].
  List<T> array<T>(T Function() item) {
    final n = uint();
    if (n > _maxArrayLength) throw const IpcDesync();
    return [for (var i = 0; i < n; i++) item()];
  }

  /// Fails unless the whole frame has been read.
  void end() {
    if (_left != 0) throw const IpcDesync();
  }
}

/// Writes the compact-encoding 3.5.2 values of one frame.
class _Writer {
  final List<int> _out = <int>[];

  Uint8List take() => Uint8List.fromList(_out);

  /// The shortest form of [n] that fits, as compact-encoding writes it.
  void uint(int n) {
    if (n < 0 || n > _maxSafeUint) {
      throw ArgumentError.value(n, 'n', 'is not a safe unsigned integer');
    }
    if (n <= 0xfc) {
      _out.add(n);
    } else if (n <= 0xffff) {
      _out.add(0xfd);
      _le(n, 2);
    } else if (n <= 0xffffffff) {
      _out.add(0xfe);
      _le(n, 4);
    } else {
      _out.add(0xff);
      _le(n & 0xffffffff, 4);
      _le(n ~/ 0x100000000, 4);
    }
  }

  void _le(int n, int bytes) {
    for (var i = 0; i < bytes; i++) {
      _out.add((n >> (8 * i)) & 0xff);
    }
  }

  void bool_(bool v) => _out.add(v ? 1 : 0);

  void string(String s) {
    final bytes = utf8.encode(s);
    uint(bytes.length);
    _out.addAll(bytes);
  }

  void fixed32(Uint8List b) {
    if (b.length != 32) {
      throw ArgumentError.value(b.length, 'appKey', 'must be 32 bytes');
    }
    _out.addAll(b);
  }

  void array<T>(List<T> items, void Function(T) item) {
    uint(items.length);
    items.forEach(item);
  }

  void port(PortBinding p) {
    string(p.service);
    uint(p.port);
  }

  void serviceEntry(ServiceEntry s) {
    string(s.name);
    uint(s.kind);
  }

  void lan(LanAddresses l) {
    array(l.addresses, string);
    uint(l.port);
  }

  void nat(NatInfo n) {
    string(n.host);
    uint(n.port);
    bool_(n.firewalled);
    bool_(n.randomized);
  }

  void vpnService(VpnService s) {
    string(s.host);
    string(s.service);
    string(s.address);
    uint(s.port);
  }

  void vpnAddress(VpnAddress a) {
    string(a.host);
    string(a.service);
    string(a.address);
    string(a.name);
  }
}
