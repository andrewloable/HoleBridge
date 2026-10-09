// Local TCP listeners per service for the app engine: one listener per service that is not udp, on
// 127.0.0.1 or, when the user shares with the network, on 0.0.0.0. Each accepted socket opens its service
// through the open callback and its bytes flow both ways. Rules: docs/cli.md (the app, Local ports, Share
// with my network) and docs/architecture.md (Bind, Forward). udp services are handled by lib/udp.js.

const TCP = require('bare-tcp')

function noop() {}

// listen binds server to port on host and resolves once it listens. bare-tcp reports a bind failure as an
// 'error' event, not a throw. Errors after the promise has settled find no effect.
function listen(server, port, host) {
  return new Promise((resolve, reject) => {
    server.on('error', reject)
    server.listen(port, host, () => resolve())
  })
}

// closeEntry stops a listener and destroys the sockets it accepted. The server closes only when every
// connection has closed, so the sockets go first.
function closeEntry(entry) {
  entry.closing = true
  for (const socket of [...entry.sockets]) socket.destroy()
  return new Promise((resolve) => entry.server.close(() => resolve()))
}

class Listeners {
  #open
  #host
  #entries = new Map() // service name -> { server, port, remembered, sockets, closing }
  #handlers = new Map() // event name -> [fn]
  #queue = Promise.resolve()

  // options: { open(service) -> Promise<Duplex>, bind: '127.0.0.1' | '0.0.0.0' }. Any bind other than
  // 0.0.0.0 means 127.0.0.1, so a typo cannot expose the services.
  constructor(options) {
    this.#open = options.open
    this.#host = options.bind === '0.0.0.0' ? '0.0.0.0' : '127.0.0.1'
  }

  // on(name, fn) adds a listener for an event. The engine emits 'error' when an open rejects, with the
  // error that open rejected with, so its code is kept.
  on(name, fn) {
    this.#handlers.set(name, [...(this.#handlers.get(name) || []), fn])
    return this
  }

  // emit calls the listeners of name. With none, an error is dropped instead of thrown, so a rejected open
  // with nobody listening cannot crash the engine.
  emit(name, ...args) {
    for (const fn of this.#handlers.get(name) || []) fn(...args)
  }

  // set(services, remembered) makes the listeners match services: a listener for each service that is not
  // udp, and the listeners of services no longer listed are closed. services is [{ name, kind, port }];
  // remembered is { [name]: port }. Resolves to { [name]: boundPort } for those services.
  async set(services, remembered = {}) {
    return this.#serial(() => this.#apply(services, remembered))
  }

  // address(name) returns { address, family, port } of the listener of a service, or null when it has none.
  address(name) {
    const entry = this.#entries.get(name)
    return entry ? entry.server.address() : null
  }

  // close() stops every listener and destroys the sockets they accepted.
  async close() {
    return this.#serial(async () => {
      for (const name of [...this.#entries.keys()]) await this.#stop(name)
    })
  }

  // serial runs fn after every earlier set and close, so two calls cannot start the same listener twice.
  #serial(fn) {
    const run = this.#queue.then(fn)
    this.#queue = run.catch(noop)
    return run
  }

  async #apply(services, remembered) {
    const wanted = new Map()
    for (const service of services) if (service.kind !== 'udp') wanted.set(service.name, service)
    for (const name of [...this.#entries.keys()]) if (!wanted.has(name)) await this.#stop(name)

    const ports = {}
    for (const [name, service] of wanted) {
      const port = remembered[name]
      const entry = this.#entries.get(name)
      // A remembered port that changed moves the listener. A listener that already has the port, or that
      // fell back to another port because the remembered one was taken, stays.
      if (entry && port !== undefined && port !== entry.remembered && port !== entry.port) {
        await this.#stop(name)
      }
      if (!this.#entries.has(name)) await this.#start(service, port)
      ports[name] = this.#entries.get(name).port
    }
    return ports
  }

  // start binds a listener for service: the remembered port, then the port hint, then a port the OS picks.
  // A port that is taken or not allowed falls back to the next choice.
  async #start(service, remembered) {
    const choices = [remembered, service.port, 0].filter(
      (port, i, all) => Number.isInteger(port) && all.indexOf(port) === i
    )
    let failure = null
    for (const port of choices) {
      const entry = { server: TCP.createServer(), port: 0, remembered, sockets: new Set(), closing: false }
      entry.server.on('connection', (socket) => this.#accept(entry, service.name, socket))
      try {
        await listen(entry.server, port, this.#host)
      } catch (err) {
        failure = err
        continue
      }
      entry.port = entry.server.address().port
      this.#entries.set(service.name, entry)
      return
    }
    throw failure
  }

  // accept opens the service for an accepted socket and pipes the bytes both ways. A socket that arrives
  // while its listener closes is destroyed, and so is one that closes while its open is pending.
  async #accept(entry, name, socket) {
    if (entry.closing) {
      socket.destroy()
      return
    }
    entry.sockets.add(socket)
    let stream = null
    let gone = false
    socket.on('error', noop) // an error is always followed by close, which does the cleanup
    socket.once('close', () => {
      gone = true
      entry.sockets.delete(socket)
      if (stream) stream.destroy()
    })

    try {
      stream = await this.#open(name)
    } catch (err) {
      socket.destroy()
      this.emit('error', err)
      return
    }
    if (gone || entry.closing) {
      stream.destroy()
      socket.destroy()
      return
    }
    stream.on('error', () => socket.destroy())
    socket.pipe(stream)
    stream.pipe(socket)
  }

  async #stop(name) {
    const entry = this.#entries.get(name)
    this.#entries.delete(name)
    await closeEntry(entry)
  }
}

module.exports = { Listeners }
