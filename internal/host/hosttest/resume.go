package hosttest

import (
	"errors"
	"fmt"
)

// Drop ends the transport of c without a close, as a network drop does. The streams of c stall, keeping
// their bytes, and wait for Reconnect.
func (c *Client) Drop() error {
	c.sess.Detach()
	return c.cur.stream.Close()
}

// Reconnect dials the host again with the key pair of the first connection, opens a new holebridge channel
// and resumes the streams that Drop left stalled. It needs a client made by Connect, since only that one
// knows the host to dial.
func (c *Client) Reconnect() error {
	if c.dht == nil {
		return errors.New("hosttest: Reconnect needs a client made by Connect")
	}
	conn, err := dial(c.dht, c.hostPub, c.clientKP)
	if err != nil {
		return fmt.Errorf("hosttest: reconnect: %w", err)
	}
	sess, k, err := open(conn)
	if err != nil {
		return err
	}
	if err := sess.Adopt(c.sess); err != nil {
		k.close()
		return err
	}
	c.sess, c.cur = sess, k
	return nil
}
