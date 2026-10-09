// Secure storage of hosts, application keys and per-host state (docs/architecture.md#state-status-and-logs,
// docs/security.md#rules-for-implementers rule 4). Values are stored as JSON strings in a SecureBackend.
import 'dart:convert';
import 'dart:math';
import 'dart:typed_data';

import '../keys/normalize.dart';
import 'secure_backend.dart';

/// A host the app knows. [key] is the normalized 9-symbol key; [appKeyIndex] is the position of the
/// host's application key in [HostStore.appKeys].
class Host {
  const Host({required this.id, required this.name, required this.key, required this.appKeyIndex});

  final String id;
  final String name;
  final String key;
  final int appKeyIndex;
}

/// Hosts, application keys and per-host state, kept in a [SecureBackend]. Every read goes to the
/// backend, so a store over the same backend sees the same state.
///
/// Each host record carries its application key (base64), so the distinct application keys are
/// derived from the records: removing the last host that uses a key drops the key too.
class HostStore {
  HostStore(this._backend);

  final SecureBackend _backend;
  Future<void> _lastWrite = Future<void>.value();

  static const _hostsKey = 'hosts';
  static const _relayKeyKey = 'relayKey';
  static String _stateKey(String hostId) => 'host.$hostId';

  /// Every host, in the order it was added.
  Future<List<Host>> hosts() async => _toHosts(await _records());

  /// Adds a host. [key] is normalized as typed input; a second host with the same key is refused
  /// with a StateError that does not name the key.
  Future<Host> addHost({
    required String name,
    required String key,
    required Uint8List appKey,
  }) => _write(() async {
    final normalized = normalizeKey(key);
    final records = await _records();
    if (records.any((record) => record.key == normalized)) {
      throw StateError('this host is already added');
    }
    final added = _Record(_newId(), name, normalized, base64Encode(appKey));
    await _saveRecords([...records, added]);
    return _toHosts([...records, added]).last;
  });

  /// Removes a host and its ports, cert pins, services cache and kinds, LAN addresses and port, and VPN
  /// addresses.
  Future<void> removeHost(String id) => _write(() async {
    await _saveRecords((await _records()).where((record) => record.id != id).toList());
    await _backend.delete(_stateKey(id));
  });

  Future<void> rename(String id, String name) => _write(() async {
    final records = await _records();
    _find(records, id).name = name;
    await _saveRecords(records);
  });

  /// The distinct application keys held, in the order first added.
  Future<List<Uint8List>> appKeys() async => [
    for (final encoded in _distinctAppKeys(await _records())) base64Decode(encoded),
  ];

  /// The cached service names of a host; empty when none are cached.
  Future<List<String>> servicesCache(String hostId) async => (await _state(hostId)).services;

  Future<void> saveServices(String hostId, List<String> services) =>
      _update(hostId, (state) => state.services = List<String>.of(services));

  /// The service kinds of a host, by service name, in the wire numbering (spec/ipc.md, Encoding).
  /// Kept beside the names cache, and empty when none are saved, as in a state file written before
  /// kinds were stored.
  Future<Map<String, int>> serviceKinds(String hostId) async => (await _state(hostId)).kinds;

  /// Saves the kinds of a host's services. The controller saves them with the names cache, at the
  /// same moments.
  Future<void> saveServiceKinds(String hostId, Map<String, int> kinds) =>
      _update(hostId, (state) => state.kinds = Map<String, int>.of(kinds));

  /// The local port of each service of a host, by service name.
  Future<Map<String, int>> ports(String hostId) async => (await _state(hostId)).ports;

  Future<void> savePort(String hostId, String service, int port) =>
      _update(hostId, (state) => state.ports[service] = port);

  Future<List<String>> lanAddresses(String hostId) async => (await _state(hostId)).lan;

  Future<void> saveLan(String hostId, List<String> addresses) =>
      _update(hostId, (state) => state.lan = List<String>.of(addresses));

  /// The LAN port the host reported with its addresses (the lan struct of connect), or 0 when none is
  /// saved.
  Future<int> lanPort(String hostId) async => (await _state(hostId)).lanPort;

  Future<void> saveLanPort(String hostId, int port) =>
      _update(hostId, (state) => state.lanPort = port);

  /// Whether Share with my network is on for a host: its listeners bind 0.0.0.0 instead of 127.0.0.1
  /// (docs/cli.md#the-app). False until it is saved, and for a state file written before the setting.
  Future<bool> shared(String hostId) => throw UnimplementedError();

  /// Saves the Share with my network setting of a host. Refuses an unknown host with a StateError.
  Future<void> saveShared(String hostId, bool shared) => throw UnimplementedError();

  /// The certificate pin of one service of a host, or null when none is set.
  Future<String?> certPins(String hostId, String service) async =>
      (await _state(hostId)).pins[service];

  Future<void> savePin(String hostId, String service, String pin) =>
      _update(hostId, (state) => state.pins[service] = pin);

  Future<void> removePin(String hostId, String service) =>
      _update(hostId, (state) => state.pins.remove(service));

  /// The relay key the app uses, or null when none is set. Passing null clears it.
  Future<String?> relayKey() async {
    final raw = await _backend.read(_relayKeyKey);
    return raw == null ? null : jsonDecode(raw) as String;
  }

  Future<void> saveRelayKey(String? key) => _write(() async {
    if (key == null) {
      await _backend.delete(_relayKeyKey);
    } else {
      await _backend.write(_relayKeyKey, jsonEncode(normalizeKey(key)));
    }
  });

  Future<List<String>> vpnAddresses(String hostId) async => (await _state(hostId)).vpn;

  Future<void> saveVpnAddresses(String hostId, List<String> addresses) =>
      _update(hostId, (state) => state.vpn = List<String>.of(addresses));

  Future<List<_Record>> _records() async {
    final raw = await _backend.read(_hostsKey);
    if (raw == null) return [];
    return [
      for (final item in jsonDecode(raw) as List) _Record.fromJson(item as Map<String, dynamic>),
    ];
  }

  Future<void> _saveRecords(List<_Record> records) =>
      _backend.write(_hostsKey, jsonEncode([for (final record in records) record.toJson()]));

  Future<_HostState> _state(String hostId) async {
    final raw = await _backend.read(_stateKey(hostId));
    return _HostState.fromJson(raw == null ? {} : jsonDecode(raw) as Map<String, dynamic>);
  }

  /// Changes one host's state. Refuses an unknown host, so no state outlives its host.
  Future<void> _update(String hostId, void Function(_HostState state) change) => _write(() async {
    _find(await _records(), hostId);
    final state = await _state(hostId);
    change(state);
    await _backend.write(_stateKey(hostId), jsonEncode(state.toJson()));
  });

  /// Runs writes one after another, so two read-modify-write calls cannot lose each other's change.
  Future<T> _write<T>(Future<T> Function() write) {
    final done = _lastWrite.then((_) => write());
    _lastWrite = done.then<void>((_) {}, onError: (_) {});
    return done;
  }

  _Record _find(List<_Record> records, String id) => records.firstWhere(
    (record) => record.id == id,
    orElse: () => throw StateError('no such host'),
  );

  static List<Host> _toHosts(List<_Record> records) {
    final appKeys = _distinctAppKeys(records);
    return [
      for (final record in records)
        Host(
          id: record.id,
          name: record.name,
          key: record.key,
          appKeyIndex: appKeys.indexOf(record.appKey),
        ),
    ];
  }

  static List<String> _distinctAppKeys(List<_Record> records) => {
    for (final record in records) record.appKey,
  }.toList();

  static String _newId() {
    final random = Random.secure();
    return [
      for (var i = 0; i < 16; i++) random.nextInt(256).toRadixString(16).padLeft(2, '0'),
    ].join();
  }
}

/// A host as stored: its id, name, normalized key and base64 application key.
class _Record {
  _Record(this.id, this.name, this.key, this.appKey);

  factory _Record.fromJson(Map<String, dynamic> json) => _Record(
    json['id'] as String,
    json['name'] as String,
    json['key'] as String,
    json['appKey'] as String,
  );

  final String id;
  String name;
  final String key;
  final String appKey;

  Map<String, dynamic> toJson() => {'id': id, 'name': name, 'key': key, 'appKey': appKey};
}

/// The per-host state that is not the host record: services and their kinds, ports, cert pins, LAN
/// addresses and port, and VPN addresses. Kinds and the LAN port are read as none when a state file
/// does not have them.
class _HostState {
  _HostState.fromJson(Map<String, dynamic> json)
    : services = List<String>.from(json['services'] as List? ?? const []),
      kinds = Map<String, int>.from(json['kinds'] as Map? ?? const {}),
      lanPort = (json['lanPort'] as int?) ?? 0,
      ports = Map<String, int>.from(json['ports'] as Map? ?? const {}),
      pins = Map<String, String>.from(json['pins'] as Map? ?? const {}),
      lan = List<String>.from(json['lan'] as List? ?? const []),
      vpn = List<String>.from(json['vpn'] as List? ?? const []);

  List<String> services;
  Map<String, int> kinds;
  int lanPort;
  final Map<String, int> ports;
  final Map<String, String> pins;
  List<String> lan;
  List<String> vpn;

  Map<String, dynamic> toJson() => {
    'services': services,
    'kinds': kinds,
    'lanPort': lanPort,
    'ports': ports,
    'pins': pins,
    'lan': lan,
    'vpn': vpn,
  };
}
