package udx

// ID returns the local stream id the stream was made with. It is the id the peer puts in the
// header of the packets it sends to this stream.
func (st *Stream) ID() uint32 {
	return st.localID
}
