// Mapping links to a service's real address onto the tunnel (docs/architecture.md#the-in-app-browser,
// Links that point at the service's real address).
import 'dart:io';

/// What the mapping knows about one host: its LAN addresses, its services and whether VPN mode is on.
class OriginContext {
  const OriginContext({
    required this.lanAddresses,
    required this.services,
    required this.vpnMode,
  });

  /// The host's own LAN addresses, as sent for the LAN route. Empty when the LAN route is off.
  final List<String> lanAddresses;

  /// The host's services, each with its port hint, listed origins, bound local port and VPN name.
  final List<ServiceOrigin> services;

  /// True when VPN mode is on, so a mapping goes to the service's VPN name, not to 127.0.0.1.
  final bool vpnMode;
}

/// One service of a host, as the mapping sees it.
class ServiceOrigin {
  const ServiceOrigin({
    required this.name,
    required this.kind,
    required this.portHint,
    required this.origins,
    required this.localPort,
    required this.vpnName,
  });

  /// The service's name in host.json.
  final String name;

  /// https, http, tcp, udp or unknown (docs/architecture.md#service-kinds). The mapped URL's
  /// scheme follows it.
  final String kind;

  /// The service's port on the host, from the handshake's port hint. A link to a host LAN address
  /// at this port is a link to the service.
  final int portHint;

  /// The origins the host owner listed for this service, such as "https://jellyfin.example".
  /// Empty for a service with none listed.
  final List<String> origins;

  /// The local port bound for the service, or null when none is bound.
  final int? localPort;

  /// The service's VPN name, such as "jellyfin.living-room.internal".
  final String vpnName;
}

/// The URL to open instead of [target] when [target] points at a service's real address: a host
/// LAN address at a service's port hint, or one of a service's listed origins. The result is that
/// service's local address (`127.0.0.1:<port>`, or its VPN name with VPN mode on), with the same
/// path, query and fragment. Null means [target] is not a service's address, so it opens normally.
///
/// Only http and https links map, only to a web service (kind http or https), and never a loopback
/// or unspecified address, even if lanAddresses lists it. Null is also returned when the matching
/// service has no address to map to: no bound local port and VPN mode off, or a VPN name that is
/// not a valid host name. Never throws.
Uri? mapNavigation(Uri target, OriginContext ctx) {
  if (target.scheme != 'http' && target.scheme != 'https') return null;
  if (target.host.isEmpty || _isLocalOnly(target.host)) return null;
  for (final service in ctx.services) {
    if (service.kind != 'http' && service.kind != 'https') continue;
    if (!_isAddressOf(target, service, ctx.lanAddresses)) continue;
    return _addressOf(service, target, ctx.vpnMode);
  }
  return null;
}

/// True when [target] is [service]'s own address: a host LAN address at its port hint, or one of
/// its listed origins on the same scheme, host and port (default ports applied).
bool _isAddressOf(
  Uri target,
  ServiceOrigin service,
  List<String> lanAddresses,
) {
  if (lanAddresses.contains(target.host) && target.port == service.portHint) {
    return true;
  }
  for (final origin in service.origins) {
    final listed = Uri.tryParse(origin);
    if (listed != null &&
        listed.scheme == target.scheme &&
        listed.host == target.host &&
        listed.port == target.port) {
      return true;
    }
  }
  return false;
}

/// The address [target] maps to for [service]: its VPN name with VPN mode on, otherwise 127.0.0.1
/// on its bound local port. Null when there is no such address, and also when the VPN name is not
/// a valid host name or is a local-only name (the name comes from the host, so it is checked here).
Uri? _addressOf(ServiceOrigin service, Uri target, bool vpnMode) {
  if (vpnMode) {
    final name = service.vpnName;
    if (!_hostName.hasMatch(name) || _isLocalOnly(name)) return null;
    return _mapped(service.kind, name, null, target);
  }
  final port = service.localPort;
  if (port == null || port < 1 || port > 65535) return null;
  return _mapped(service.kind, '127.0.0.1', port, target);
}

/// [target]'s path, query and fragment on [scheme] and [host]. User info is dropped, so no
/// credentials from the link reach the tunnel. Null when [host] is not a valid host name, so a bad
/// name never reaches Uri, which would throw or build a URL with no host.
Uri? _mapped(String scheme, String host, int? port, Uri target) {
  if (!_hostName.hasMatch(host)) return null;
  return Uri(
    scheme: scheme,
    host: host,
    port: port,
    path: target.path,
    query: target.hasQuery ? target.query : null,
    fragment: target.hasFragment ? target.fragment : null,
  );
}

/// A valid host name: dot-separated labels of lower-case letters, digits and hyphens, each label
/// starting with a letter or digit. A label may end with a hyphen, because a service name may. Any
/// other character (such as an upper-case letter, / : @ ? # or a space) is refused. Upper case is
/// refused on purpose: VPN names are built lower-cased (spec/ipc.md point 7, internal/config
/// ValidServiceName), so refusing it fails closed and cannot break a valid name.
final _hostName = RegExp(r'^[a-z0-9][a-z0-9-]*(\.[a-z0-9][a-z0-9-]*)*$');

/// True for localhost names and for loopback or unspecified addresses (127.0.0.0/8, ::1, 0.0.0.0,
/// ::), including their IPv4-mapped IPv6 forms (::ffff:127.0.0.1, ::ffff:0.0.0.0). These reach the
/// phone's own machine, so a link to one is never a host LAN address. Odd numeric spellings of an
/// IPv4 address (127.1, 2130706433, 0x7f.1, 0177.0.0.1) are refused too, because a WebView reads
/// them as IPv4 addresses and Dart does not.
bool _isLocalOnly(String host) {
  final name = host.endsWith('.') ? host.substring(0, host.length - 1) : host;
  if (name == 'localhost' || name.endsWith('.localhost')) return true;
  if (!name.contains(':') &&
      _endsInNumber.hasMatch(name.split('.').last) &&
      !_canonicalIPv4.hasMatch(name)) {
    return true;
  }
  final address = InternetAddress.tryParse(name);
  if (address == null) return false;
  var bytes = address.rawAddress;
  if (bytes.length == 16 &&
      bytes.sublist(0, 10).every((b) => b == 0) &&
      bytes[10] == 0xff &&
      bytes[11] == 0xff) {
    bytes = bytes.sublist(12); // IPv4-mapped: judge the embedded IPv4 address.
  }
  return address.isLoopback ||
      bytes.every((b) => b == 0) ||
      (bytes.length == 4 && bytes[0] == 127);
}

/// The last label of a host that a WebView may read as a number: decimal, or hex with 0x.
final _endsInNumber = RegExp(r'^(\d+|0[xX][0-9a-fA-F]*)$');

/// A plain dotted-decimal IPv4 address, as Dart and a WebView both read it.
final _canonicalIPv4 = RegExp(r'^(0|[1-9]\d{0,2})(\.(0|[1-9]\d{0,2})){3}$');
