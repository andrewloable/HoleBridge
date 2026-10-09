// The firewall of a stream that is not connected yet. Its behaviour follows libudx's firewall callback and udx-native's
// raw stream firewall (Apache License 2.0, Copyright (c) 2021 Holepunch Inc.); the code is pears-go's own.

package udx

import "net"

// SetFirewall sets fn as the hook for the packets that arrive while st is not connected. For each such packet, fn is
// called with the address the packet came from. A true answer lets the packet through, and fn is expected to have
// connected st to that address (Connect) by then, as upstream's raw stream firewall does. A false answer drops the
// packet, and the stream stays unconnected for the next one. A stream connected by any path never calls fn. A stream
// without a hook takes every packet, as before.
func (st *Stream) SetFirewall(fn func(from *net.UDPAddr) bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.firewall = fn
}

// admit reports whether run hands pk to handle. A connected stream, a torn-down one and one without a hook admit every
// packet. An unconnected stream asks its hook, which may connect the stream, so the hook runs without st.mu held. A
// packet from an address that is not a UDP address is refused.
func (st *Stream) admit(pk Packet) bool {
	st.mu.Lock()
	fn := st.firewall
	open := fn == nil || st.peer != nil || st.err != nil
	st.mu.Unlock()
	if open {
		return true
	}
	from, ok := pk.Addr.(*net.UDPAddr)
	return ok && fn(from)
}
