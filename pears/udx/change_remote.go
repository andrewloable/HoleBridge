// Ported from libudx udx.c (udx_stream_change_remote at ae8bff7, and the remote_changing ack check in process_packet),
// Apache License 2.0, Copyright (c) 2021 Holepunch Inc.

package udx

import (
	"errors"
	"net"
)

// errRemoteChanging is the error of a ChangeRemote while an earlier change still waits for its old path to be acked
// (udx-native's "Remote already changing").
var errRemoteChanging = errors.New("udx: remote already changing")

// errBadRemote is the error of a ChangeRemote to an address or socket that cannot be a remote.
var errBadRemote = errors.New("udx: not a valid remote")

// ChangeRemote moves the connected stream to the peer's stream id remoteID at addr. When socket is not the stream's
// own, the stream moves onto it, and the old socket keeps routing the stream's packets, as libudx routes a stream by
// its id. Only where new packets go changes: a packet that was sent before the change goes to the remote it was
// first sent to until the peer acks it, as in libudx, since the peer may still be on the old path.
//
// The returned channel closes once every packet sent before the change is acked by the peer (libudx's remote_changed
// callback). It is already closed when nothing was in flight. A caller that keeps an old path open for its packets,
// such as a relay, closes it when the channel closes. A change while an earlier one is pending fails.
func (st *Stream) ChangeRemote(socket *Socket, remoteID uint32, addr *net.UDPAddr) (<-chan struct{}, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.tick()
	if st.err != nil {
		return nil, st.err
	}
	if st.peer == nil {
		return nil, errNotConnected
	}
	if st.changed != nil {
		return nil, errRemoteChanging
	}
	if socket == nil || addr == nil || addr.Port < 1 || addr.Port > 65535 || addr.IP.To16() == nil {
		return nil, errBadRemote
	}
	if socket.isClosed() {
		return nil, net.ErrClosed
	}

	if old := st.sock; socket != old {
		if err := socket.attach(st.localID, st.ch); err != nil {
			return nil, err
		}
		old.handOver(st.localID, st.ch)
		st.aliased = append(st.aliased, old)
		st.sock = socket
	}

	st.remoteID = remoteID
	st.peer = &net.UDPAddr{IP: append(net.IP(nil), addr.IP...), Port: addr.Port, Zone: addr.Zone}

	changed := make(chan struct{})
	if st.seq == st.remoteAcked {
		close(changed)
		return changed, nil
	}
	st.changed = changed
	st.changeSeq = st.seq
	return changed, nil
}

// finishChange closes the channel of a pending change once the peer has acked every packet sent before it. The caller
// holds mu.
func (st *Stream) finishChange() {
	if st.changed == nil || seqCompare(st.remoteAcked, st.changeSeq) < 0 {
		return
	}
	close(st.changed)
	st.changed = nil
}

// releaseOldPath is run when the stream is torn down: a change still pending is done, and the sockets it left stop
// routing to the stream. The caller holds mu.
func (st *Stream) releaseOldPath() {
	if st.changed != nil {
		close(st.changed)
		st.changed = nil
	}
	for _, s := range st.aliased {
		s.unalias(st.localID, st.ch)
	}
	st.aliased = nil
}
