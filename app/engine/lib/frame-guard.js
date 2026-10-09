// Frame guard for the secret stream data path (lib/connect.js, openSession, which both routes share).
// Upstream protomux 3.12.1 decodes a control batch inside a control batch by recursing, with no limit. One
// peer frame of nested batches overflows the JS stack (RangeError), and safety-catch rethrows a RangeError,
// so the process dies. Upstream never sends a nested batch, so the frame is refused here, as the Go side
// refuses it (pears/protomux, errFrame): the stream is destroyed before protomux decodes the frame.
// The first level is read the way protomux's _onbatch reads it (the same reads, the same entry windows), so
// the guard and protomux agree on every frame.

const Protomux = require('protomux')
const c = require('compact-encoding')

// nestedControlBatch(buffer) -> boolean: true when buffer is a control batch (remote id 0, type 0) with an
// entry that is another control batch. It reads one level and does not recurse: the nested batch is refused
// at that level, so no deeper level is ever read. It never throws. A frame it cannot read is not reported
// as nested, and protomux then fails it as it fails any malformed frame.
function nestedControlBatch(buffer) {
  try {
    const state = { buffer, start: 0, end: buffer.byteLength }
    const remoteId = c.uint.decode(state)
    const type = c.uint.decode(state)
    if (remoteId !== 0 || type !== 0) return false
    const end = state.end
    let entryRemoteId = c.uint.decode(state)
    while (state.end > state.start) {
      const len = c.uint.decode(state)
      if (len === 0) {
        entryRemoteId = c.uint.decode(state)
        continue
      }
      // The entry's window, set as protomux sets it: len bytes, cut at the end of the batch.
      state.end = Math.min(state.start + len, end)
      if (entryRemoteId === 0 && c.uint.decode(state) === 0) return true
      state.start = state.end
      state.end = end
    }
    return false
  } catch {
    return false
  }
}

// GuardedProtomux is a Protomux that checks each frame before protomux decodes it. The stream's data listener
// calls _ondata with every frame, so the check runs before any of protomux's reads of that frame.
class GuardedProtomux extends Protomux {
  _ondata(buffer) {
    if (nestedControlBatch(buffer)) {
      // Destroyed with an error, as protomux destroys a stream on a malformed frame. The message names the
      // fault only; it carries no bytes of the frame.
      this._safeDestroy(new Error('protomux: a control batch inside a control batch'))
      return
    }
    super._ondata(buffer)
  }
}

// guardedProtomux(stream) -> the Protomux of stream, guarded. A stream that already has an unguarded
// Protomux is refused, because its frames would not be checked.
function guardedProtomux(stream) {
  const existing = stream.userData
  if (existing && existing.isProtomux) {
    if (existing instanceof GuardedProtomux) return existing
    throw new Error('the stream already has a Protomux that is not guarded')
  }
  return new GuardedProtomux(stream)
}

module.exports = { nestedControlBatch, guardedProtomux }
