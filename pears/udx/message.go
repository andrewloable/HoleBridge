// Ported from libudx udx.h (UDX_HEADER_MESSAGE) and the unordered send and receive paths of udx.c,
// Apache License 2.0, Copyright (c) 2021 Holepunch Inc. The C source is not in this tree: the packet
// size follows the udx-native 1.21.3 measurements in docs/spike-m1.md (a send of n bytes is one
// packet of n+20 bytes, and the peer does not ack it).
package udx

import "errors"

// streamMTU is the stream's mtu in udx-native 1.21.3: the largest UDX packet, header included.
const streamMTU = 1200

// maxMessage is the largest message that fits one packet. A message is sent without padding, so the
// whole packet after its header is payload. The session's secret-stream layer takes 24 bytes of it
// (inferred from sizes in docs/spike-m1.md), so the app's largest datagram is 1156 bytes.
const maxMessage = streamMTU - headerSize

var errMessageTooLarge = errors.New("udx: message larger than MaxMessage")

// SendMessage sends b as one unordered, unreliable UDX packet of type MESSAGE. The peer does not
// ack it and it is never retransmitted, so it may arrive out of order or not at all. It returns an
// error if b is longer than MaxMessage.
func (st *Stream) SendMessage(b []byte) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return st.err
	}
	if st.peer == nil {
		return errNotConnected
	}
	if len(b) > maxMessage {
		return errMessageTooLarge
	}
	return st.send(FlagMessage, st.nextSeq, b)
}

// Messages returns the channel that carries the messages the peer sends with SendMessage, as they
// arrive. The channel is closed when the stream is torn down. A message that finds the queue full
// is dropped and counted in the socket's Dropped.
func (st *Stream) Messages() <-chan []byte {
	return st.msgs
}

// MaxMessage returns the largest payload that fits one UDX packet.
func (st *Stream) MaxMessage() int {
	return maxMessage
}

// onMessage queues a message from the peer for Messages. A message has no seq and is not acked, so
// it leaves the stream's ordered state alone.
func (st *Stream) onMessage(b []byte) {
	select {
	case st.msgs <- b:
	default:
		st.sock.dropped.Add(1)
	}
}
