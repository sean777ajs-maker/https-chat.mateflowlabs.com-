package main

// A small RFC 6455 WebSocket server on the standard library: unfragmented
// frames out, reassembled messages in, with keepalive pings, a read deadline
// and a cap on message size.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	wsContinuation = 0x0
	wsText         = 0x1
	wsBinary       = 0x2
	wsClose        = 0x8
	wsPing         = 0x9
	wsPong         = 0xA

	wsMaxMessage = 1 << 20          // largest inbound message accepted
	wsPingEvery  = 25 * time.Second // under the usual 60s proxy idle timeout
	wsReadWait   = 70 * time.Second // a peer silent this long, pongs included, is gone
	wsWriteWait  = 10 * time.Second

	// Fixed by RFC 6455 for deriving Sec-WebSocket-Accept. The SHA-1 it is
	// hashed with is mandated there and protects nothing.
	wsAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex // the sender and the reader's pong replies both write
}

// wsUpgrade completes the opening handshake and takes over the connection.
// On failure the HTTP error has already been written.
func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || key == "" ||
		!headerHasToken(r.Header, "Connection", "upgrade") ||
		!headerHasToken(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket handshake expected", http.StatusBadRequest)
		return nil, errors.New("websocket: not a handshake")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("websocket: unsupported version")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unavailable", http.StatusInternalServerError)
		return nil, errors.New("websocket: connection cannot be hijacked")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		http.Error(w, "websocket unavailable", http.StatusInternalServerError)
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsAcceptGUID))
	_, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	if err == nil {
		err = rw.Flush()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return &wsConn{conn: conn, br: rw.Reader}, nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h[http.CanonicalHeaderKey(name)] {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// ReadMessage returns the next complete data message, reassembling fragments.
// Control frames are dealt with here: pings are answered and a close is echoed.
func (c *wsConn) ReadMessage() (opcode int, payload []byte, err error) {
	for {
		c.conn.SetReadDeadline(time.Now().Add(wsReadWait))
		var head [2]byte
		if _, err := io.ReadFull(c.br, head[:]); err != nil {
			return 0, nil, err
		}
		fin, op := head[0]&0x80 != 0, int(head[0]&0x0f)
		size := uint64(head[1] & 0x7f)
		// No extension is negotiated, so the reserved bits must be clear, and
		// every frame from a client must be masked.
		if head[0]&0x70 != 0 || head[1]&0x80 == 0 {
			return 0, nil, c.fail(1002)
		}
		switch size {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			size = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return 0, nil, err
			}
			size = binary.BigEndian.Uint64(ext[:])
		}
		control := op >= wsClose
		if control && (!fin || size > 125) {
			return 0, nil, c.fail(1002)
		}
		if size > wsMaxMessage || uint64(len(payload))+size > wsMaxMessage {
			return 0, nil, c.fail(1009)
		}
		var mask [4]byte
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return 0, nil, err
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(c.br, data); err != nil {
			return 0, nil, err
		}
		for i := range data {
			data[i] ^= mask[i&3]
		}
		switch op {
		case wsClose:
			c.writeFrame(wsClose, data)
			return 0, nil, io.EOF
		case wsPing:
			if err := c.writeFrame(wsPong, data); err != nil {
				return 0, nil, err
			}
		case wsPong:
		case wsContinuation:
			if opcode == 0 {
				return 0, nil, c.fail(1002)
			}
			payload = append(payload, data...)
		case wsText, wsBinary:
			if opcode != 0 {
				return 0, nil, c.fail(1002)
			}
			opcode, payload = op, data
		default:
			return 0, nil, c.fail(1002)
		}
		if !control && fin {
			return opcode, payload, nil
		}
	}
}

// WriteMessage sends one unfragmented frame.
func (c *wsConn) WriteMessage(opcode int, payload []byte) error {
	return c.writeFrame(opcode, payload)
}

func (c *wsConn) writeFrame(opcode int, payload []byte) error {
	frame := make([]byte, 0, len(payload)+10)
	frame = append(frame, 0x80|byte(opcode)) // FIN set; server frames are never masked
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, byte(n))
	case n <= 0xffff:
		frame = append(frame, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		frame = append(append(frame, 127), ext[:]...)
	}
	frame = append(frame, payload...)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	_, err := c.conn.Write(frame)
	return err
}

// fail tells the peer why the connection is ending and reports the error.
func (c *wsConn) fail(status int) error {
	c.writeFrame(wsClose, []byte{byte(status >> 8), byte(status)})
	return errors.New("websocket: closed with status " + strconv.Itoa(status))
}

func (c *wsConn) Close() error { return c.conn.Close() }
