// The app controller: starts the engine, connects hosts, and applies the engine's events to what the
// screens show (spec/ipc.md, docs/architecture.md#app-engine-ipc, docs/architecture.md#sessions-and-reconnects).
import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter_pear_bare/flutter_pear_bare.dart' show WorkletCrash;

import 'engine/engine_client.dart';
import 'engine/engine_host.dart';
import 'engine/ipc_codec.dart';
import 'keys/normalize.dart';
import 'store/host_store.dart';

/// What the app shows for one host.
class HostView {
  const HostView({required this.route, required this.services, this.lastErrorCode});

  /// lan, direct, relay, looking or unreachable. Empty when no session is up and no search runs.
  final String route;

  /// The services the host shares, each with the local port bound for it.
  final List<ServiceView> services;

  /// The last HB- code that a reject or a failed connect gave for this host, or null. A route that is
  /// up clears it, and so does a connect that succeeds.
  final String? lastErrorCode;
}

/// One service of a host, as the view shows it.
class ServiceView {
  const ServiceView({required this.name, required this.kind, this.port});

  final String name;

  /// https, http, tcp, udp or unknown (docs/architecture.md#service-kinds).
  final String kind;

  /// The local port bound for the service, or null when none is bound. Nothing is bound before a
  /// session is up, so the view from the cache has none.
  final int? port;
}

/// The route and the engine's NAT view for one host, from its status reply (spec/ipc.md, status).
class HostStatus {
  const HostStatus({required this.route, required this.nat});

  /// lan, direct, relay, looking or unreachable. Empty when no session is up and no search runs.
  final String route;

  /// The engine's NAT view, or null when the engine sent none (a host it does not have).
  final NatInfo? nat;
}

/// Drives the app engine for the screens. It starts the engine, connects hosts, and applies the
/// engine's events to each host's view, saving the services cache, the service kinds, the ports and
/// the LAN addresses and port the engine reports.
///
/// The engine host is started by [start], which then builds the EngineClient on the started host.
/// The client cannot exist before that, because it listens to the host's frames when it is made.
///
/// Engine failures that a request raises (a StateError when the engine is not running, a
/// TimeoutException when it does not answer) are not caught here: connect and addTypedKey rethrow
/// them. A reply with ok false is a result, not a failure: it sets the host's last error code, or
/// the controller's [lastErrorCode] when no host is involved.
class AppController extends ChangeNotifier {
  AppController(this._host, this._store);

  final EngineHost _host;
  final HostStore _store;

  EngineClient? _client;
  StreamSubscription<IpcEvent>? _eventSubscription;

  /// The listener on the current worklet's crashes, or null when none is set: before start, after a
  /// restart that failed, and after dispose. Each start replaces it (see _listenCrashes).
  StreamSubscription<WorkletCrash>? _crashSubscription;

  /// The restart in flight, or null when none runs. Overlapping calls share it (see restartEngine).
  Future<void>? _restarting;

  /// Set by dispose. A restart still in flight then neither notifies nor listens to crashes.
  bool _disposed = false;

  /// The hosts as the store held them at the last load. An event names its host, so the name maps to
  /// an id through this list.
  List<Host> _hosts = const [];

  final Map<String, _ViewState> _views = {};

  /// The connect in progress for each host id, so a second connect for the host waits for the first.
  final Map<String, Future<void>> _connecting = {};

  /// The hosts the engine has registered: a connect replied ok, or it timed out and the engine keeps
  /// the host registered and retrying until close (spec/ipc.md, connect). Cleared when the engine
  /// restarts, because a restarted engine has no registrations.
  final Set<String> _registered = {};

  /// Whether the relay request has been sent since the engine last started.
  bool _relaySent = false;

  /// The relay request in flight, or null when none is. Overlapping calls share it (see _ensureRelay).
  Future<IpcReply?>? _relayInFlight;

  /// The name of the addTypedKey connect in flight, or null when none runs. The engine names the host by
  /// this name before the host is saved, so its events cannot be mapped to an id yet.
  String? _typedName;

  /// The LAN addresses and port the engine reported for [_typedName] in the current try. The engine sends
  /// them before the connect reply, so they are kept until the host is saved.
  ({List<String> addresses, int port})? _typedLan;

  /// The end of the addTypedKey calls queued so far, so that each call runs after the one before it. It
  /// never completes with an error, so a call that failed does not stop the calls after it.
  Future<void> _typedQueue = Future<void>.value();

  /// The end of the setShared calls queued so far, so that each call runs after the one before it. It
  /// never completes with an error, so a call that failed does not stop the calls after it.
  Future<void> _shareQueue = Future<void>.value();

  String? _lastErrorCode;

  /// The last HB- code that no single host owns: an error event, a relay that was refused, or a failed
  /// addTypedKey. Null when there is none.
  String? get lastErrorCode => _lastErrorCode;

  /// Starts the engine host, then builds the client on it and listens to its events before any request
  /// is sent, because an event with no listener is dropped. Loads the hosts with their cached services
  /// and kinds, and sends the relay request when a relay key and an application key are held. It listens
  /// to the worklet's crashes (see restartEngine). Calling it again starts the host again if it stopped,
  /// and keeps the client.
  Future<void> start() async {
    await _host.start();
    if (_client == null) {
      final client = EngineClient(_host, onRestart: _listenCrashes);
      _eventSubscription = client.events.listen(_onEvent);
      _client = client;
    }
    _listenCrashes();
    await _loadHosts();
    await _ensureRelay();
    notifyListeners();
  }

  /// Restarts the engine: the one way the app restarts it. The controller resets what a restarted engine
  /// lost (the registrations and the relay), then stops and starts the host, then listens to the new
  /// worklet's crashes. A crash from EngineHost.crashes calls this. Overlapping calls share one restart.
  /// A restart that fails sets [lastErrorCode] to HB-ENGINE-DOWN and does not throw. Calling it before
  /// [start] throws StateError. After this, every engine restart goes through the controller; the client's
  /// own restart after a desync is separate and already handled (see _onEngineRestart).
  Future<void> restartEngine() {
    if (_client == null) throw StateError('start() has not run');
    final running = _restarting;
    if (running != null) return running;
    final attempt = _restart().whenComplete(() {
      _restarting = null;
    });
    _restarting = attempt;
    return attempt;
  }

  /// One restart, for restartEngine. It never fails: a failed stop or start sets HB-ENGINE-DOWN.
  Future<void> _restart() async {
    _onEngineRestart();
    try {
      await _host.stop();
      await _host.start();
      _listenCrashes();
      if (_lastErrorCode == 'HB-ENGINE-DOWN') _lastErrorCode = null;
    } catch (_) {
      _lastErrorCode = 'HB-ENGINE-DOWN';
    }
    _notify();
  }

  /// Listens to the current worklet's crashes, in place of any earlier listener. Called after every
  /// start: the controller's own, the restart of restartEngine, and the client's restart after a desync
  /// (EngineClient's onRestart). A crash restarts the engine.
  void _listenCrashes() {
    if (_disposed) return;
    _cancelCrashes();
    _crashSubscription = _host.crashes.listen(_onCrash);
  }

  void _cancelCrashes() {
    unawaited(_crashSubscription?.cancel());
    _crashSubscription = null;
  }

  /// The worklet exited without the app stopping it. The done event of the stream is the stop that closes
  /// it, and is not a crash.
  void _onCrash(WorkletCrash _) {
    if (_disposed) return;
    unawaited(restartEngine());
  }

  /// Notifies the listeners, unless the controller was disposed while a restart ran.
  void _notify() {
    if (!_disposed) notifyListeners();
  }

  /// Makes sure a session to the host [hostId] is up: sends connect with the host's key, the application
  /// key it was saved with, its saved LAN addresses and port, its remembered ports, and bind 127.0.0.1
  /// (0.0.0.0 when Share with my network is on for the host).
  /// A host the engine already has registered is not sent again, so a port or bind changed since then
  /// reaches the engine only through reconnect. The request waits 75 s, longer than the engine's 60 s
  /// lookup, so the engine's own reply comes first.
  Future<void> connect(String hostId) {
    final running = _connecting[hostId];
    if (running != null) return running;
    return _track(hostId, _connect(hostId));
  }

  /// Closes the host in the engine, then connects it again with the settings the store holds now
  /// (remembered ports, saved LAN addresses, bind). connect() sends nothing for a host the engine
  /// already has, so this is how a changed port or bind reaches the engine. Closes under the name the
  /// store holds now (a rename is not handled here). It waits for the connect or reconnect in flight
  /// for the host, and a connect() made meanwhile waits for this one. An unknown id throws StateError
  /// and sends nothing.
  Future<void> reconnect(String hostId) => _track(hostId, _reconnect(hostId, _connecting[hostId]));

  /// Asks the engine for the route and the engine's NAT view of the host [hostId] (spec/ipc.md, status).
  /// The status event that answers the request carries both, and it is taken whether it arrives before
  /// the reply or after it. A reply with ok false (HB-USAGE: the engine does not have the host connected)
  /// gives route "" and no NAT view. It sets no error on the host, because a status query for a host that
  /// is not connected is not a failure of the host. An id the store does not have throws StateError and
  /// sends nothing, and so does a call before start, as connect does. Engine failures are not caught here.
  Future<HostStatus> status(String hostId) async {
    final host = await _loadHost(hostId);
    final client = _client;
    if (client == null) throw StateError('start() has not run');
    // Listens before the request is sent, so the event cannot pass by unheard.
    final answer = Completer<StatusEvent>();
    final listener = client.events.listen((event) {
      if (event is StatusEvent && event.host == host.name && !answer.isCompleted) answer.complete(event);
    });
    try {
      final reply = await _send(StatusRequest(host: host.name));
      if (!reply.ok) return const HostStatus(route: '', nat: null);
      final event = await answer.future.timeout(_requestTimeout);
      return HostStatus(route: event.route, nat: event.nat);
    } finally {
      await listener.cancel();
    }
  }

  /// What the screens show for the host [hostId]. An id the controller does not know has an empty view.
  HostView view(String hostId) {
    final state = _views[hostId];
    if (state == null) return const HostView(route: '', services: []);
    return HostView(
      route: state.route,
      services: List.unmodifiable(state.services),
      lastErrorCode: state.lastErrorCode,
    );
  }

  /// Connects a host typed as 9 symbols (case, dashes and spaces do not matter), and adds it when it
  /// answers.
  ///
  /// The held application keys are tried one at a time, in the order of HostStore.appKeys(). Each try
  /// connects under one default name that no stored host uses ("Host", "Host 2", ...), and every failed
  /// try is closed under that name: a timed-out host stays registered and retrying in the engine
  /// (spec/ipc.md, connect). After HB-LOOKUP-TIMEOUT the next key is tried (spec/ipc.md decided point 5).
  /// Any other failed reply ends the tries. The first key that connects is saved with the new host under
  /// the same name, so the engine's registration matches the saved host. The LAN addresses and port the
  /// engine reports for that name before the reply are saved with the host too.
  ///
  /// Returns the new host's id. Returns the id of a host already added with this key, without contacting
  /// the engine. Returns null when no held key reaches a host, and sets [lastErrorCode] to the last
  /// failure's code. Returns null with HB-KEY-INVALID when the key does not normalize. With no held
  /// application key, it returns null without contacting the engine. Calls run one at a time, so a call
  /// starts after the previous one has finished, whether it returned, returned null or threw.
  Future<String?> addTypedKey(String typed) {
    final run = _typedQueue.then((_) => _addTypedKey(typed));
    _typedQueue = run.then<void>((_) {}, onError: (Object _) {});
    return run;
  }

  /// Sets the relay key the app uses: a key typed as 9 symbols (case, dashes and spaces do not matter),
  /// or null, or blank text, for no relay. A key is stored with HostStore.saveRelayKey, and the relay
  /// request is sent at once, even when one was sent since the engine started (spec/ipc.md, relay). It
  /// carries the first held application key, as start() sends it (spec/ipc.md decided point 6); with
  /// no application key held it sends nothing, and the first connect sends it. No relay is an empty key
  /// with a zero application key, and it is sent whether or not an application key is held.
  ///
  /// Throws KeyFormatException, storing and sending nothing, when the text is not blank and does not
  /// normalize. A relay request that fails is not caught here, as in connect: the key stays stored, and
  /// the next connect sends it. A reply with ok false sets [lastErrorCode], as start() does.
  ///
  /// Returns the code of the relay reply when that reply has ok false, so the caller can show this save's
  /// refusal. Returns null in every other case: the reply is ok, or no relay request was sent because no
  /// application key is held. It does not return the older [lastErrorCode].
  Future<String?> setRelayKey(String? typed) async {
    final key = (typed == null || typed.trim().isEmpty) ? null : normalizeKey(typed);
    await _store.saveRelayKey(key);
    // A relay request in flight carries the old key. Let it finish, so the new one goes after it.
    final running = _relayInFlight;
    if (running != null) {
      try {
        await running;
      } catch (_) {
        // Its failure is not this call's. The request below is sent anyway.
      }
    }
    if (key == null) {
      final reply = await _send(RelayRequest(key: '', appKey: Uint8List(32)));
      _relaySent = true;
      if (!reply.ok) {
        _lastErrorCode = reply.code;
        notifyListeners();
        return reply.code;
      }
      return null;
    }
    _relaySent = false;
    final reply = await _ensureRelay();
    return reply != null && !reply.ok ? reply.code : null;
  }

  /// Turns Share with my network on or off for the host [hostId] (docs/cli.md#the-app). The setting is
  /// stored with HostStore.saveShared and applies to every later connect: bind 0.0.0.0 when it is on, and
  /// 127.0.0.1 when it is off.
  ///
  /// When the engine has the host registered, the host is closed and connected again at once, with the
  /// new bind: a connect for a host the engine already has gives HB-USAGE, so it is close, then connect
  /// (spec/ipc.md, connect). The reconnect waits for a connect in progress for the host. A host that is
  /// not registered is not connected: only the setting changes. Calls run one at a time, in the order
  /// they were made, so the last call decides the bind. Throws StateError, sending nothing, for a host
  /// the store does not have. Engine failures are not caught here, as in connect.
  Future<void> setShared(String hostId, bool shared) {
    final run = _shareQueue.then((_) => _setShared(hostId, shared));
    _shareQueue = run.then<void>((_) {}, onError: (Object _) {});
    return run;
  }

  Future<void> _setShared(String hostId, bool shared) async {
    await _store.saveShared(hostId, shared);
    // A connect in progress read the old setting. Let it finish, so the reconnect below replaces it.
    final connecting = _connecting[hostId];
    if (connecting != null) {
      try {
        await connecting;
      } catch (_) {
        // Its failure belongs to whoever called connect.
      }
    }
    if (!_registered.contains(hostId)) return;
    final host = await _loadHost(hostId);
    await _send(CloseRequest(host: host.name));
    _registered.remove(hostId);
    await connect(hostId);
  }

  Future<String?> _addTypedKey(String typed) async {
    final appKeys = await _store.appKeys();
    if (appKeys.isEmpty) return null;
    String key;
    try {
      key = normalizeKey(typed);
    } on KeyFormatException {
      return _fail('HB-KEY-INVALID');
    }
    for (final host in await _store.hosts()) {
      if (host.key == key) return host.id;
    }

    // The relay goes first: the typed host may be behind a NAT that is reached only through the relay, so
    // the relay key must be in place before the dial (spec/ipc.md, relay).
    await _ensureRelay();
    final name = await _unusedName();
    _typedName = name;
    try {
      var failure = _lookupTimeout;
      for (final appKey in appKeys) {
        // Each try starts with no LAN event: a late one from an earlier try must not reach the host.
        _typedLan = null;
        final reply = await _send(
          ConnectRequest(
            host: name,
            key: key,
            appKey: appKey,
            lan: const LanAddresses(addresses: [], port: 0),
            ports: const [],
            bind: _loopback,
          ),
          timeout: _connectTimeout,
        );
        if (reply.ok) {
          final added = await _store.addHost(name: name, key: key, appKey: appKey);
          _registered.add(added.id);
          await _loadHosts();
          final lan = _typedLan;
          if (lan != null) {
            _save(() => _store.saveLan(added.id, lan.addresses));
            _save(() => _store.saveLanPort(added.id, lan.port));
          }
          _applyConnectReply(added.id, reply);
          return added.id;
        }
        failure = reply.code;
        await _send(CloseRequest(host: name));
        if (reply.code != _lookupTimeout) break;
      }
      return _fail(failure);
    } finally {
      // The name and the kept event are for this connect only, on every exit, including a failure.
      _typedName = null;
      _typedLan = null;
    }
  }

  Future<void> _connect(String hostId) async {
    await _ensureRelay();
    if (_registered.contains(hostId)) return;
    final host = await _loadHost(hostId);
    _applyConnectReply(hostId, await _sendConnect(host));
  }

  /// One reconnect, for reconnect. It waits for [pending], the connect or reconnect in flight for the host,
  /// and ignores how it ends. Then it closes the host (the engine replies ok even for a host it does not
  /// have, so the close always goes out) and connects it again with the store as it is now.
  Future<void> _reconnect(String hostId, Future<void>? pending) async {
    if (pending != null) {
      try {
        await pending;
      } catch (_) {
        // Only the end of the pending attempt matters here, not how it ended.
      }
    }
    final host = await _loadHost(hostId);
    await _send(CloseRequest(host: host.name));
    _registered.remove(hostId);
    // The relay goes first, as in connect. After an engine restart the engine has no relay, so this sends it.
    await _ensureRelay();
    _applyConnectReply(hostId, await _sendConnect(host));
  }

  /// Builds the connect request for [host] from the store as it is now (the application key it was saved
  /// with, its saved LAN addresses and port, its remembered ports, and the bind) and sends it. connect and
  /// reconnect both send through here, so a setting the request gains later reaches both.
  Future<IpcReply> _sendConnect(Host host) async {
    final appKeys = await _store.appKeys();
    final addresses = await _store.lanAddresses(host.id);
    final lanPort = await _store.lanPort(host.id);
    final ports = await _store.ports(host.id);
    final shared = await _store.shared(host.id);
    return _send(
      ConnectRequest(
        host: host.name,
        key: host.key,
        appKey: appKeys[host.appKeyIndex],
        lan: LanAddresses(addresses: addresses, port: lanPort),
        ports: [for (final entry in ports.entries) PortBinding(service: entry.key, port: entry.value)],
        bind: shared ? _allInterfaces : _loopback,
      ),
      timeout: _connectTimeout,
    );
  }

  /// Records [attempt] as the work in progress for [hostId] until it completes, and returns it. A reconnect
  /// that starts meanwhile takes over the entry, so the earlier attempt must not remove it on completion.
  Future<void> _track(String hostId, Future<void> attempt) {
    _connecting[hostId] = attempt;
    return attempt.whenComplete(() {
      if (identical(_connecting[hostId], attempt)) _connecting.remove(hostId);
    });
  }

  /// Applies a connect reply to the host's view. An ok reply sets the route, services and ports, as the
  /// events do, and clears the host's error. A reply with ok false sets the host's error and throws
  /// nothing.
  void _applyConnectReply(String hostId, IpcReply reply) {
    if (reply.ok || reply.code == _lookupTimeout) {
      // After a lookup timeout the engine keeps the host registered and retries until close.
      _registered.add(hostId);
    }
    final view = _viewOf(hostId);
    if (reply.ok) {
      view.lastErrorCode = null;
      view.route = reply.route;
      _applyServices(hostId, reply.services, reply.ports);
    } else {
      view.lastErrorCode = reply.code;
    }
    notifyListeners();
  }

  /// Sends the relay request once per engine start: the relay key with the first held application key,
  /// because the relay is set once for the app (spec/ipc.md decided point 6). It sends nothing while no
  /// relay key or no application key is held. Connect and addTypedKey call it before their first dial, so
  /// the relay key is in place before any host is dialled. A call that overlaps a request in flight waits
  /// for that request instead of sending its own. A request that fails leaves the relay unsent, so the
  /// next call tries again. It completes with the relay reply, or with null when it sent nothing.
  Future<IpcReply?> _ensureRelay() {
    final running = _relayInFlight;
    if (running != null) return running;
    final attempt = _sendRelay().whenComplete(() {
      _relayInFlight = null;
    });
    _relayInFlight = attempt;
    return attempt;
  }

  /// Sends the relay request unless one was sent since the engine started, and completes with its reply.
  /// It completes with null when it sends nothing (no relay key or no application key is held, or the
  /// relay was sent already).
  Future<IpcReply?> _sendRelay() async {
    if (_relaySent) return null;
    final relayKey = await _store.relayKey();
    final appKeys = await _store.appKeys();
    if (relayKey == null || appKeys.isEmpty) return null;
    final reply = await _send(RelayRequest(key: relayKey, appKey: appKeys.first));
    _relaySent = true;
    if (!reply.ok) {
      _lastErrorCode = reply.code;
      notifyListeners();
    }
    return reply;
  }

  /// Sends a request through the client and waits for its reply. The client's failures are not caught.
  Future<IpcReply> _send(IpcRequest request, {Duration timeout = _requestTimeout}) {
    final client = _client;
    if (client == null) throw StateError('start() has not run');
    return client.request(request, timeout: timeout);
  }

  /// Reloads the hosts. Each host's cached services and kinds are read the first time it is seen, so
  /// the view is right before any session is up. An event that set the services first is newer, and
  /// is kept.
  Future<void> _loadHosts() async {
    _hosts = await _store.hosts();
    for (final host in _hosts) {
      final view = _viewOf(host.id);
      if (view.cacheLoaded) continue;
      view.cacheLoaded = true;
      final names = await _store.servicesCache(host.id);
      final kinds = await _store.serviceKinds(host.id);
      if (view.services.isEmpty) {
        view.services = [for (final name in names) ServiceView(name: name, kind: _kindName(kinds[name] ?? 0))];
      }
    }
  }

  Future<Host> _loadHost(String hostId) async {
    _hosts = await _store.hosts();
    for (final host in _hosts) {
      if (host.id == hostId) return host;
    }
    throw StateError('no such host');
  }

  /// The first of "Host", "Host 2", "Host 3", ... that no stored host has.
  Future<String> _unusedName() async {
    final taken = {for (final host in await _store.hosts()) host.name};
    var name = 'Host';
    for (var n = 2; taken.contains(name); n++) {
      name = 'Host $n';
    }
    return name;
  }

  /// The id of the stored host called [name]: the first match, since names may repeat. Null when none.
  String? _idOf(String name) {
    for (final host in _hosts) {
      if (host.name == name) return host.id;
    }
    return null;
  }

  _ViewState _viewOf(String hostId) => _views.putIfAbsent(hostId, _ViewState.new);

  /// Sets the route of the host [id] from a route or status event. A live route clears the host's error.
  void _setRoute(String id, String route) {
    final view = _viewOf(id);
    view.route = route;
    if (route == 'lan' || route == 'direct' || route == 'relay') view.lastErrorCode = null;
  }

  /// Sets the services of a host from a services list (a services event, or a connect reply that is
  /// ok), and saves them: the names and kinds as the cache, and each bound port.
  void _applyServices(String hostId, List<ServiceEntry> list, List<PortBinding> ports) {
    final bound = {for (final port in ports) port.service: port.port};
    _viewOf(hostId).services = [
      for (final entry in list) ServiceView(name: entry.name, kind: _kindName(entry.kind), port: bound[entry.name]),
    ];
    _save(() => _store.saveServices(hostId, [for (final entry in list) entry.name]));
    _save(() => _store.saveServiceKinds(hostId, {for (final entry in list) entry.name: entry.kind}));
    for (final port in ports) {
      _save(() => _store.savePort(hostId, port.service, port.port));
    }
  }

  /// Runs a store write that an event or a reply cannot wait for. A write for a host removed in the
  /// meantime is dropped. Any other failure is raised as an uncaught error.
  void _save(Future<void> Function() write) {
    unawaited(
      write().catchError((Object error) {
        if (error is! StateError) throw error;
      }),
    );
  }

  void _onEvent(IpcEvent event) {
    switch (event) {
      case RouteEvent(:final host, :final route):
        final id = _idOf(host);
        if (id != null) _setRoute(id, route);
        break;
      case StatusEvent(:final host, :final route):
        // The status event carries the route as a route event does, so the screens agree. status() takes the
        // NAT view from the same event.
        final id = _idOf(host);
        if (id != null) _setRoute(id, route);
        break;
      case SessionEvent(:final host, :final up):
        // A session that goes down leaves no route: no session is up and no search runs, until the
        // engine says otherwise (spec/ipc.md, status). But the engine reports looking before this event
        // when a session drops, because a search runs then. So only a live route is blanked here;
        // looking and unreachable stay as the route event set them.
        final id = _idOf(host);
        if (id != null && !up) {
          final view = _viewOf(id);
          if (view.route == 'lan' || view.route == 'direct' || view.route == 'relay') view.route = '';
        }
        break;
      case ServicesEvent(:final host, :final list, :final ports):
        final id = _idOf(host);
        if (id != null) _applyServices(id, list, ports);
        break;
      case RejectEvent(:final host, :final code):
        final id = _idOf(host);
        if (id != null) _viewOf(id).lastErrorCode = code;
        break;
      case LanEvent(:final host, :final addresses, :final port):
        final id = _idOf(host);
        if (id != null) {
          _save(() => _store.saveLan(id, addresses));
          _save(() => _store.saveLanPort(id, port));
        } else if (host == _typedName) {
          // The host of an addTypedKey connect is not saved until its reply: keep the event for then.
          _typedLan = (addresses: addresses, port: port);
        }
        break;
      case ErrorEvent(:final code):
        // The error event names no host, so it is the controller's error and never a host's.
        _lastErrorCode = code;
        if (code == 'HB-IPC-DESYNC') _onEngineRestart();
        break;
      default:
        break;
    }
    notifyListeners();
  }

  /// The engine is about to restart, after a desync or in restartEngine, and it has no registrations, no
  /// relay and no route. The next connect registers each host again, and the next request sends the relay
  /// again. The crash listener is cancelled before the stop; the restart listens again once the new worklet
  /// runs.
  void _onEngineRestart() {
    _cancelCrashes();
    _registered.clear();
    _relaySent = false;
    for (final view in _views.values) {
      view.route = '';
    }
  }

  /// The error of a failed try, set as the controller's last error. Returns null, as addTypedKey does.
  String? _fail(String code) {
    _lastErrorCode = code;
    notifyListeners();
    return null;
  }

  @override
  void dispose() {
    _disposed = true;
    _cancelCrashes();
    unawaited(_eventSubscription?.cancel());
    super.dispose();
  }
}

/// The state of one host that its view is built from.
class _ViewState {
  String route = '';
  List<ServiceView> services = const [];
  String? lastErrorCode;

  /// Whether the cached services were read for this host, so they are read once.
  bool cacheLoaded = false;
}

/// The view name of a wire service kind (spec/ipc.md, Encoding; docs/architecture.md#service-kinds):
/// 1 https, 2 http, 3 tcp, 4 udp. Zero, and any number the wire does not name, is unknown.
String _kindName(int kind) => switch (kind) {
  1 => 'https',
  2 => 'http',
  3 => 'tcp',
  4 => 'udp',
  _ => 'unknown',
};

/// The timeout of every request but connect. The engine answers each one, and its own connect search
/// gives up at 60 s.
const _requestTimeout = Duration(seconds: 60);

/// The timeout of connect: longer than the engine's 60 s lookup, so its HB-LOOKUP-TIMEOUT reply
/// arrives before the client's own timeout.
const _connectTimeout = Duration(seconds: 75);

/// The bind address of a connect when Share with my network is off, which is the default
/// (spec/ipc.md, connect). addTypedKey always uses it: a host that is not saved yet has no setting.
const _loopback = '127.0.0.1';

/// The bind address of a connect when Share with my network is on for the host (spec/ipc.md, connect).
const _allInterfaces = '0.0.0.0';

/// The code of a connect whose search found no host in 60 s.
const _lookupTimeout = 'HB-LOOKUP-TIMEOUT';
