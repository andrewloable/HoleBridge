// Stream multiplexer of the app engine: streams, credit flow control, the receive budget, the close
// handshake, the stream limits and the malformed-input rules of protocol v1 (docs/architecture.md, "Flow
// control", "Limits" and "Malformed input"). The Go host has the same design in internal/mux, and the two
// must agree.

const { Duplex } = require('streamx')
const b4a = require('b4a')
const { REJECT } = require('./protocol.js')

const MIB = 1024 * 1024

// MAX_CHUNK is the largest data payload this side sends: the 64 KiB wire limit, the same as the Go host's
// maxChunk (internal/mux). The frame of a full payload is about 65544 bytes, which pears/protomux reads whole.
const MAX_CHUNK = 64 * 1024

// CLOSED_GRACE is how long (ms) the id of a finished or refused stream is remembered. Messages that raced
// its close (window, close, opened, reject) are ignored in that time instead of ending the session.
const CLOSED_GRACE = 60 * 1000

const RESET = new Error('stream reset')
const CLOSED = new Error('stream closed')
const CANCELLED = new Error('open cancelled')
const SESSION_CLOSED = new Error('session closed')

function noop() {}

// SessionError is what receive throws when the session must close.
class SessionError extends Error {}

// RejectError is what open rejects with when the host refuses the stream.
class RejectError extends Error {
  constructor(code, reason) {
    super(`stream rejected (code ${code}): ${reason}`)
    this.code = code
    this.reason = reason
  }
}

// sub returns a minus b, or 0 when b is larger.
function sub(a, b) {
  return b > a ? 0 : a - b
}

// copyOf returns b in a buffer of its own. A decoded payload is a view into the frame, and the frame into
// the transport's read buffer, so a view kept here would pin that whole buffer. Go copies the bytes too.
function copyOf(b) {
  const c = b4a.allocUnsafeSlow(b.length)
  c.set(b)
  return c
}

// Budget is the receive budget of a process, shared by its sessions: the credit granted to streams
// that their local readers have not yet taken, up to total bytes (64 MiB in the app). A stream's first
// grant is min(window, share), where share is the budget left divided by the open streams. A grant made
// when a reader takes bytes is capped by the stream's own window and by the same share, so once the
// budget is spent a stream gets credit only as the budget is drained. A stream left with no credit is
// starved: it waits in starved, and every time credit is freed the budget tops those streams up.
class Budget {
  constructor(total) {
    this.total = total
    this.spent = 0 // credit granted and not yet taken by a reader, over all streams
    this.open = 0 // streams counted in the budget
    this.starved = new Set() // streams with no credit for their peer, topped up when credit frees
  }

  share() {
    if (this.spent >= this.total) return 0
    const left = this.total - this.spent
    return this.open <= 0 ? left : Math.floor(left / this.open)
  }

  // take grants up to room from the share and counts it as spent. It does not top up, so a top-up
  // cannot start another one.
  take(room) {
    const g = Math.min(room, this.share())
    this.spent += g
    return g
  }

  // admit counts a new stream and returns its first grant.
  admit(window) {
    this.open++
    return this.take(window)
  }

  // refill records that a reader took released bytes of a stream, then returns the stream's new grant,
  // at most room and at most its share.
  refill(released, room) {
    this.spent = sub(this.spent, released)
    const g = this.take(room)
    this.topUp()
    return g
  }

  // retire drops a stream from the budget, with the credit it had not yet been taken.
  retire(untaken) {
    this.open--
    this.spent = sub(this.spent, untaken)
    this.topUp()
  }

  // topUp grants the starved streams what the share now allows.
  topUp() {
    for (const st of [...this.starved]) st.topUp()
  }
}

// Counter is the stream count shared by the sessions of a process (Config Limits in internal/mux). A
// session takes a slot when it opens or accepts a stream and gives it back when the stream ends.
class Counter {
  constructor(limit) {
    this.limit = limit
    this.used = 0 // stream slots held
  }

  take() {
    if (this.used >= this.limit) return false
    this.used++
    return true
  }

  give() {
    if (this.used > 0) this.used--
  }
}

// Session is one protocol v1 session: its streams, their credit and their share of the budget.
// send(index, message) carries a message to the other side. accept(service) is called on the host for
// each open; a non-zero code in its result rejects the stream, and target(stream) gets the host's side
// of an accepted stream after opened is sent. clock() returns the time in milliseconds (Date.now by
// default) and only times the memory of finished ids.
class Session {
  constructor({ role, send, window = 2 * MIB, maxStreams = 128, counter, budget, accept, clock = Date.now }) {
    this.role = role
    this.send = send
    this.window = window
    this.maxStreams = maxStreams
    this.counter = counter
    this.budget = budget
    this.accept = accept
    this.clock = clock
    this.streams = new Map() // the streams the session still holds
    this.gone = new Map() // ids finished or refused, and when (ms)
    this.slots = 0 // streams holding a slot of this session
    this.closed = false
    this.lastId = 0
  }

  // receive takes one decoded message from the other side. It throws SessionError when the session
  // must close; a fault on one stream resets that stream and does not throw.
  receive(index, message) {
    switch (index) {
      case 0: return this.onOpen(message)
      case 1: return this.onOpened(message)
      case 2: return this.onReject(message)
      case 3: return this.onData(message)
      case 4: return this.onWindow(message)
      case 5: return this.onClose(message)
    }
    throw new SessionError(`message ${index} is not implemented yet`)
  }

  // open opens a stream to service and resolves with it, or rejects when the host refuses it. A stream
  // the session or the process has no room for is refused here with code 2, and nothing is sent. An
  // abort of signal before opened cancels the open. Only the app role opens streams.
  async open(service, { signal } = {}) {
    if (this.role !== 'app') throw new Error('only the app role opens streams')
    if (this.closed) throw SESSION_CLOSED
    if (signal && signal.aborted) throw CANCELLED
    if (!this.takeSlot()) throw new RejectError(REJECT.limitReached, 'limit reached')
    const id = ++this.lastId
    const st = new Stream(this, id)
    st.slot = true
    st.limit = this.budget.admit(this.window)
    st.track()
    this.streams.set(id, st)
    const opened = new Promise((resolve, reject) => {
      st.pending = { resolve, reject }
    })
    if (signal) {
      // The listener is removed when the open settles, so a signal that outlives the open keeps nothing.
      const onAbort = () => this.cancelOpen(st)
      const detach = () => signal.removeEventListener('abort', onAbort)
      signal.addEventListener('abort', onAbort, { once: true })
      opened.then(detach, detach)
    }
    this.send(0, { stream: id, service, window: st.limit })
    return opened
  }

  // cancelOpen gives up an open that is still waiting: the stream is dropped, its slot and credit go back,
  // and close tells the host to free its side. A late opened or reject for it is then ignored.
  cancelOpen(st) {
    if (!st.pending) return
    this.forget(st)
    st.stop(CANCELLED)
    if (st.finishWrite()) this.send(5, { stream: st.id })
  }

  onOpen(m) {
    if (this.role !== 'host') throw new SessionError('the app side got an open message')
    if (this.closed) throw new SessionError('session closed')
    if (this.streams.has(m.stream) || this.recent(m.stream)) {
      throw new SessionError(`open for stream ${m.stream}, which is already open`)
    }
    if (!this.takeSlot()) {
      this.tombstone(m.stream)
      this.send(2, { stream: m.stream, code: REJECT.limitReached, reason: 'limit reached' })
      return
    }
    const res = this.accept(m.service)
    if (res.code) {
      this.giveSlot()
      this.tombstone(m.stream)
      this.send(2, { stream: m.stream, code: res.code, reason: res.reason })
      return
    }
    const st = new Stream(this, m.stream)
    st.slot = true
    st.sendLimit = m.window
    st.limit = this.budget.admit(this.window)
    st.track()
    this.streams.set(m.stream, st)
    this.send(1, { stream: m.stream, window: st.limit, token: b4a.alloc(16) })
    if (res.target) res.target(st)
  }

  onOpened(m) {
    const st = this.streams.get(m.stream)
    if (!st) {
      if (this.recent(m.stream)) return // the answer to a cancelled open
      throw new SessionError(`opened for stream ${m.stream}, which is not open`)
    }
    if (st.err) return // reset while opening; the answer came too late
    if (!st.pending) throw new SessionError(`opened for stream ${m.stream}, which is not waiting`)
    const { resolve } = st.pending
    st.pending = null
    st.sendLimit = m.window
    resolve(st)
  }

  onReject(m) {
    const st = this.streams.get(m.stream)
    if (!st) {
      if (this.recent(m.stream)) return // the answer to a cancelled open
      throw new SessionError(`reject for stream ${m.stream}, which is not open`)
    }
    if (st.err) return
    if (!st.pending) throw new SessionError(`reject for stream ${m.stream}, which is not waiting`)
    st.stop(new RejectError(m.code, m.reason))
    this.forget(st)
  }

  onData(m) {
    const st = this.streams.get(m.stream)
    if (!st) {
      if (this.recent(m.stream)) return
      throw new SessionError(`data for stream ${m.stream}, which was never opened`)
    }
    if (st.err) return
    const n = m.payload.length
    if (st.pending || st.rclosed || st.received + n > st.limit) {
      // Data before opened, data after the peer's close, or data past the credit: only this stream is
      // reset.
      st.reset()
      return
    }
    st.received += n
    st.chunks.push(copyOf(m.payload))
    st.wakeReader()
  }

  onWindow(m) {
    const st = this.streams.get(m.stream)
    if (!st) {
      if (this.recent(m.stream)) return
      throw new SessionError(`window for stream ${m.stream}, which was never opened`)
    }
    if (st.err) return
    st.sendLimit += m.credit
    st.wakeWriter()
  }

  // onClose handles the peer's close. It ends the local read side: the local side reads the bytes the peer
  // sent, then end. Our own close is not sent until the local side ends or destroys the stream, so the
  // local side can still write after the peer has closed (half-close). A close for a stream that never
  // opened resets it, and is answered at once.
  onClose(m) {
    const st = this.streams.get(m.stream)
    if (!st) {
      if (this.recent(m.stream)) return
      throw new SessionError(`close for stream ${m.stream}, which is not open`)
    }
    st.rclosed = true
    if (st.pending) {
      st.stop(RESET) // the peer ended the stream before opened
      if (st.finishWrite()) this.send(5, { stream: m.stream })
    } else if (st.wclosed) {
      st.complete()
    }
    st.wakeWriter()
    st.wakeReader()
  }

  // destroy ends the session: every open stream fails with an error, and no stream opens any more. It
  // sends nothing; the caller closes the channel.
  destroy() {
    if (this.closed) return
    this.closed = true
    for (const st of [...this.streams.values()]) {
      if (st.pending) st.stop(SESSION_CLOSED) // rejects the open
      else if (!st.err) {
        st.stop(SESSION_CLOSED)
        st.destroy(SESSION_CLOSED)
      }
    }
  }

  // takeSlot takes a stream slot: one of the session's maxStreams and one of the shared counter. It
  // reports false when either is full.
  takeSlot() {
    if (this.maxStreams > 0 && this.slots >= this.maxStreams) return false
    if (this.counter && !this.counter.take()) return false
    this.slots++
    return true
  }

  // giveSlot returns a slot that takeSlot took.
  giveSlot() {
    this.slots--
    if (this.counter) this.counter.give()
  }

  // recent reports whether id finished or was refused less than CLOSED_GRACE ago.
  recent(id) {
    const t = this.gone.get(id)
    return t !== undefined && this.clock() - t < CLOSED_GRACE
  }

  // tombstone remembers id as finished now, and forgets the ids that are older than CLOSED_GRACE.
  tombstone(id) {
    const now = this.clock()
    for (const [old, t] of this.gone) {
      if (now - t >= CLOSED_GRACE) this.gone.delete(old)
    }
    this.gone.set(id, now)
  }

  // forget drops the record of st and remembers its id as finished.
  forget(st) {
    this.streams.delete(st.id)
    this.tombstone(st.id)
  }
}

// Stream is one forwarded connection inside a session. Its readable side has credit: the peer may send
// up to limit bytes in all, and limit grows only as the local reader takes bytes. Its writable side is
// limited by the peer's grants. Each side closes its write side with close, and the record of the stream
// is kept until both closes have come back.
class Stream extends Duplex {
  constructor(session, id) {
    // highWaterMark 0 makes streamx read only when the local side asks for bytes, so credit follows the
    // reader and not a prefetch.
    super({ highWaterMark: 0 })
    this.id = id
    this.session = session
    this.live = true // counted in the budget
    this.slot = false // holds a slot of the session and of the counter
    this.err = null
    this.pending = null // { resolve, reject } until the open is answered (app role)
    this.limit = 0 // bytes the peer may send in all: our grants
    this.received = 0 // bytes received from the peer
    this.consumed = 0 // bytes the local reader has taken
    this.chunks = [] // received bytes not yet taken
    this.readWait = null // the streamx read callback waiting for bytes
    this.sendLimit = 0 // bytes we may send in all: the peer's grants
    this.sent = 0
    this.writeWait = null // the write waiting for credit
    this.flushes = [] // the callbacks of the writes not yet sent, in order
    this.wclosed = false // our close is sent, or the write side is closed locally
    this.rclosed = false // the peer's close came
  }

  _read(cb) {
    if (this.err) return cb(null)
    if (this.chunks.length === 0) {
      if (this.rclosed) {
        this.push(null) // the peer closed and every byte it sent has been read
        return cb(null)
      }
      this.readWait = cb
      return
    }
    const chunk = this.take(MAX_CHUNK)
    this.push(chunk)
    this.grant(chunk.length)
    cb(null)
  }

  // write takes an optional callback, as Node streams do: it runs once the whole data has been sent, or
  // with the error that stopped it. streamx's write takes no callback. A write made after destroy fails at
  // once, since streamx drops its data. The engine never calls end with data: that path skips write, and
  // would shift the callbacks out of line.
  write(data, cb) {
    const done = typeof cb === 'function' ? cb : noop
    if (this.destroying) {
      queueMicrotask(() => done(this.err))
      return false
    }
    this.flushes.push(done)
    return super.write(data)
  }

  _write(data, cb) {
    const flushed = this.flushes.shift() || noop
    this.sendData(data).then(
      () => { cb(null); flushed(null) },
      (err) => { cb(err); flushed(err) }
    )
  }

  // _final runs when end has sent the written data: the write side closes (half-close) and the local side
  // can still read.
  _final(cb) {
    if (this.finishWrite()) this.session.send(5, { stream: this.id })
    cb(null)
  }

  // _predestroy runs when destroy is called, before streamx waits for a write in progress. Ending the stream
  // here wakes a write that waits for credit, so it fails now and _destroy sends close at once.
  _predestroy() {
    this.stop(CLOSED)
  }

  // _destroy closes both directions. The writes still queued fail with the stream's error. The close goes out
  // unless the session is closed.
  _destroy(cb) {
    for (const done of this.flushes.splice(0)) done(this.err)
    if (!this.session.closed && this.finishWrite()) this.session.send(5, { stream: this.id })
    cb(null)
  }

  // pipe reads the stream into dest, and resumes it each time dest drains. With highWaterMark 0 streamx reads
  // only when asked: pipe alone never reads, and it stops reading when a write returns false, so without
  // these resumes the rest of the stream never reaches dest.
  pipe(dest, cb) {
    const out = super.pipe(dest, cb)
    this.resume()
    dest.on('drain', () => this.resume())
    return out
  }

  // take removes up to max bytes from the front of the received chunks.
  take(max) {
    const first = this.chunks[0]
    if (first.length <= max) {
      this.chunks.shift()
      return first
    }
    this.chunks[0] = first.subarray(max)
    return first.subarray(0, max)
  }

  // grant counts n bytes as taken by the local reader, then grants the peer as much again as the budget
  // and the window allow, as a window message.
  grant(n) {
    this.consumed += n
    if (!this.live) return
    this.extend(this.session.budget.refill(n, this.room()))
  }

  // room is the credit the window still allows: the window less the bytes the peer may still send.
  room() {
    return sub(this.session.window, this.limit - this.consumed)
  }

  // extend grants the peer g more bytes, as a window message when g is above 0, and keeps the stream in
  // the budget's starved set while it has no credit left.
  extend(g) {
    if (g > 0) {
      this.limit += g
      this.session.send(4, { stream: this.id, credit: g, received: this.received })
    }
    this.track()
  }

  // track keeps the stream in the budget's starved set while it has no credit for the peer, so the budget
  // tops it up when credit frees.
  track() {
    const starved = this.session.budget.starved
    if (this.live && this.limit === this.consumed) starved.add(this)
    else starved.delete(this)
  }

  // topUp grants a starved stream what the budget allows now. The budget calls it when credit frees.
  topUp() {
    if (this.live) this.extend(this.session.budget.take(this.room()))
  }

  wakeReader() {
    const cb = this.readWait
    if (cb === null) return
    this.readWait = null
    this._read(cb)
  }

  wakeWriter() {
    const attempt = this.writeWait
    if (attempt === null) return
    this.writeWait = null
    attempt()
  }

  // sendData sends data in data messages, each one waiting for the credit it needs.
  async sendData(data) {
    let n = 0
    while (n < data.length) {
      const k = await this.reserve(data.length - n)
      this.session.send(3, { stream: this.id, payload: data.subarray(n, n + k) })
      n += k
    }
  }

  // reserve waits until the peer has credit, then takes up to want bytes of it, at most one data
  // message's worth.
  reserve(want) {
    return new Promise((resolve, reject) => {
      const attempt = () => {
        if (this.err) return reject(this.err)
        if (this.wclosed) return reject(CLOSED)
        if (this.sendLimit > this.sent) {
          const k = Math.min(want, this.sendLimit - this.sent, MAX_CHUNK)
          this.sent += k
          return resolve(k)
        }
        this.writeWait = attempt
      }
      attempt()
    })
  }

  // stop ends the stream with err: the bytes not yet read are dropped, the credit and the slot go back,
  // a pending open fails, and a waiting reader and writer wake with err.
  stop(err) {
    if (this.err) return
    this.err = err
    this.chunks = []
    this.release()
    if (this.pending) {
      const { reject } = this.pending
      this.pending = null
      reject(err)
    }
    this.wakeWriter()
    this.wakeReader()
  }

  // reset ends the stream after a fault and tells the peer with close, since the protocol has no reset
  // message. A stream that is still opening has no reader yet, so it is not destroyed.
  reset() {
    const opening = this.pending !== null
    this.stop(RESET)
    if (this.finishWrite()) this.session.send(5, { stream: this.id })
    if (!opening) this.destroy(RESET)
  }

  // finishWrite closes the write side. It reports whether our close must be sent now, and completes the
  // stream when the peer's close has already come.
  finishWrite() {
    if (this.wclosed) return false
    this.wclosed = true
    if (this.rclosed) this.complete()
    return true
  }

  // complete ends a stream that both sides have closed.
  complete() {
    this.release()
    this.session.forget(this)
  }

  // release gives the stream's credit back to the budget and its slot back to the session, once.
  release() {
    if (this.live) {
      this.live = false
      this.session.budget.starved.delete(this)
      this.session.budget.retire(this.limit - this.consumed)
    }
    if (this.slot) {
      this.slot = false
      this.session.giveSlot()
    }
  }
}

// Stream is not exported: callers get streams from open, and the host gets its side from target.
module.exports = { Session, Budget, Counter, SessionError, RejectError }
