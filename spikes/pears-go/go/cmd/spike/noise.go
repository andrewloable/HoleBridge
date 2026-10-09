package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"golang.org/x/crypto/blake2b"

	"holebridge-spike-pears-go/internal/noise"
)

const (
	labelGo      = "holebridge spike noise go static"
	labelJS      = "holebridge spike noise js static"
	payloadInit  = "spike initiator payload"
	payloadResp  = "spike responder payload"
	checkInit    = "initiator-check"
	noiseTimeout = 30 * time.Second
)

// runNoise is step 3: a Noise IK handshake between the Go port and the JS noise-handshake peer
// (noise-peer.js), in the role given by -go-role. The JS peer is a child process spoken to over
// its stdin and stdout with 2-byte length frames.
func runNoise(args []string) error {
	fs := flag.NewFlagSet("noise", flag.ContinueOnError)
	node := fs.String("node", "", "path to noise-peer.js")
	goRole := fs.String("go-role", "initiator", "initiator or responder (the JS peer takes the other role)")
	nodeBin := fs.String("nodebin", "node", "node binary")
	negative := fs.Bool("negative", false, "negative control: the initiator pins a wrong responder key, so the handshake must fail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *node == "" {
		return errors.New("-node is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), noiseTimeout)
	defer cancel()

	jsRole := "responder"
	if *goRole == "responder" {
		jsRole = "initiator"
	} else if *goRole != "initiator" {
		return fmt.Errorf("bad -go-role %q", *goRole)
	}
	cmd := exec.CommandContext(ctx, *nodeBin, *node, jsRole)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rd := bufio.NewReader(stdout)
	wr := &frameWriter{w: stdin}

	goStatic := noise.KeypairFromSeed(labelSeed(labelGo))
	jsStatic := noise.KeypairFromSeed(labelSeed(labelJS))
	prologue := noise.Prologue()
	fmt.Printf("noise prologue (NS.PEER_HANDSHAKE) prefix=%x\n", prologue[:4])

	start := time.Now()
	var ok bool
	var hsErr error
	if *goRole == "initiator" {
		remote := jsStatic.Public
		if *negative {
			remote = noise.KeypairFromSeed(labelSeed("holebridge spike noise wrong key")).Public
		}
		ok, hsErr = runInitiator(goStatic, remote, wr, rd)
	} else {
		ok, hsErr = runResponder(goStatic, wr, rd)
	}
	elapsed := time.Since(start)
	stdin.Close()
	waitErr := cmd.Wait()
	if *negative && hsErr != nil {
		fmt.Printf("negative control: handshake failed as expected: %v js-exit-ok=%v\n", hsErr, waitErr == nil)
		return nil
	}
	if hsErr != nil {
		return fmt.Errorf("go side: %w", hsErr)
	}
	fmt.Printf("noise handshake and transport check go-role=%s ok=%v elapsed=%.2fms js-exit-ok=%v\n",
		*goRole, ok, float64(elapsed.Microseconds())/1000, waitErr == nil)
	if !ok || waitErr != nil {
		return errors.New("noise check failed")
	}
	return nil
}

func runInitiator(me noise.Keypair, remote [32]byte, wr *frameWriter, rd *bufio.Reader) (bool, error) {
	hs := noise.NewHandshake(true, me, remote)
	msg1, err := hs.WriteMessage1([]byte(payloadInit))
	if err != nil {
		return false, err
	}
	if err := wr.write(msg1); err != nil {
		return false, err
	}
	msg2, err := readFrame(rd)
	if err != nil {
		return false, err
	}
	pt, err := hs.ReadMessage2(msg2)
	if err != nil {
		return false, err
	}
	payloadOK := bytes.Equal(pt, []byte(payloadResp))
	// transport: send our check under Tx, the responder answers with its verdict and hash
	ck, err := noise.Seal(hs.Tx, []byte(checkInit))
	if err != nil {
		return false, err
	}
	if err := wr.write(ck); err != nil {
		return false, err
	}
	reply, err := readFrame(rd)
	if err != nil {
		return false, err
	}
	plain, err := noise.Open(hs.Rx, reply)
	if err != nil {
		return false, fmt.Errorf("transport reply did not authenticate: %w", err)
	}
	if len(plain) != 65 {
		return false, fmt.Errorf("transport reply has %d bytes", len(plain))
	}
	respAccepted := plain[0] == 1
	hashEqual := bytes.Equal(plain[1:], hs.Hash[:])
	fmt.Printf("go initiator: payload2 ok=%v, responder accepted our check=%v, handshake hash equal=%v, complete=%v\n",
		payloadOK, respAccepted, hashEqual, hs.Complete)
	return payloadOK && respAccepted && hashEqual && hs.Complete, nil
}

func runResponder(me noise.Keypair, wr *frameWriter, rd *bufio.Reader) (bool, error) {
	hs := noise.NewHandshake(false, me, [32]byte{})
	msg1, err := readFrame(rd)
	if err != nil {
		return false, err
	}
	pt, err := hs.ReadMessage1(msg1)
	if err != nil {
		return false, err
	}
	payloadOK := bytes.Equal(pt, []byte(payloadInit))
	msg2, err := hs.WriteMessage2([]byte(payloadResp))
	if err != nil {
		return false, err
	}
	if err := wr.write(msg2); err != nil {
		return false, err
	}
	// transport: read the initiator's check under Rx, answer with verdict and hash under Tx
	check, err := readFrame(rd)
	if err != nil {
		return false, err
	}
	got, openErr := noise.Open(hs.Rx, check)
	authed := openErr == nil && bytes.Equal(got, []byte(checkInit))
	verdict := byte(0)
	if authed {
		verdict = 1
	}
	reply, err := noise.Seal(hs.Tx, append([]byte{verdict}, hs.Hash[:]...))
	if err != nil {
		return false, err
	}
	if err := wr.write(reply); err != nil {
		return false, err
	}
	fmt.Printf("go responder: payload1 ok=%v, initiator check authenticated=%v, complete=%v\n", payloadOK, authed, hs.Complete)
	return payloadOK && authed && hs.Complete, nil
}

type frameWriter struct{ w io.Writer }

func (f *frameWriter) write(b []byte) error {
	if len(b) > 0xffff {
		return errors.New("frame too large")
	}
	head := make([]byte, 2)
	binary.BigEndian.PutUint16(head, uint16(len(b)))
	_, err := f.w.Write(append(head, b...))
	return err
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, err
	}
	b := make([]byte, binary.BigEndian.Uint16(head))
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// labelSeed is BLAKE2b-256 of a spike label. The spike uses it to make fixed fixture keys.
func labelSeed(label string) [32]byte {
	return blake2b.Sum256([]byte(label))
}
