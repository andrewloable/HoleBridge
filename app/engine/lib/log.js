// Redacting logger for the app engine. A secret value never reaches a log line or a string.

// The value sits in a private field, so no key, spread or JSON output can expose it. Methods live on
// the prototype, so spreading a secret copies nothing onto a log entry.
class Secret {
  #value

  constructor(value) {
    this.#value = value
  }

  reveal() {
    return this.#value
  }

  toString() {
    return '[redacted]'
  }

  toJSON() {
    return '[redacted]'
  }

  [Symbol.for('nodejs.util.inspect.custom')]() {
    return '[redacted]'
  }
}

// secret(value) wraps a value so String(), JSON.stringify() and util.inspect show [redacted];
// reveal() returns the value.
function secret(value) {
  return new Secret(value)
}

const RANK = { error: 0, warn: 1, info: 2, debug: 3 }

// createLogger({ level, write }) returns { error, warn, info, debug }. Each call writes one JSON
// line {level, msg, ...fields}; levels below level are dropped.
function createLogger({ level = 'info', write = (line) => console.error(line) } = {}) {
  const emit = (name) => (msg, fields = {}) => {
    if (RANK[name] <= RANK[level]) write(JSON.stringify({ level: name, msg, ...fields }))
  }
  return { error: emit('error'), warn: emit('warn'), info: emit('info'), debug: emit('debug') }
}

module.exports = { secret, createLogger }
