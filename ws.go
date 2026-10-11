package main

// Minimal WebSocket (RFC 6455) server side, standard library only.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	wsMaxFrame = 1 << 20 // 1 MiB (thumbnails are ~20 KB)
	wsQueue    = 256
)

var errWSClosed = errors.New("websocket closed")

type wsConn struct {
	conn      net.Conn
	rd        *bufio.Reader
	out       chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func wsUpgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("not a websocket request")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("hijack unsupported")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	c := &wsConn{conn: conn, rd: brw.Reader, out: make(chan []byte, wsQueue), done: make(chan struct{})}
	go c.writer()
	return c, nil
}

func (c *wsConn) writer() {
	for {
		select {
		case frame, ok := <-c.out:
			if !ok {
				return
			}
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := c.conn.Write(frame); err != nil {
				c.Close()
				return
			}
			if frame[0] == 0x88 { // we sent a close frame
				c.Close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func encodeFrame(opcode byte, payload []byte) []byte {
	n := len(payload)
	var head []byte
	switch {
	case n < 126:
		head = []byte{0x80 | opcode, byte(n)}
	case n < 65536:
		head = []byte{0x80 | opcode, 126, 0, 0}
		binary.BigEndian.PutUint16(head[2:], uint16(n))
	default:
		head = make([]byte, 10)
		head[0], head[1] = 0x80|opcode, 127
		binary.BigEndian.PutUint64(head[2:], uint64(n))
	}
	return append(head, payload...)
}

func (c *wsConn) enqueue(frame []byte) {
	select {
	case <-c.done:
	case c.out <- frame:
	default:
		// The client can't keep up (or is gone). Drop it rather than stall everyone.
		c.Close()
	}
}

// Send queues a JSON message. Never blocks.
func (c *wsConn) Send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.enqueue(encodeFrame(0x1, b))
}

// SendAndClose queues a message followed by a close frame.
func (c *wsConn) SendAndClose(v any) {
	c.Send(v)
	c.enqueue(encodeFrame(0x8, nil))
}

func (c *wsConn) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.conn.Close()
	})
}

// Recv returns the next text/binary message.
func (c *wsConn) Recv() ([]byte, error) {
	var msg []byte
	started := false
	for {
		var hdr [2]byte
		if _, err := io.ReadFull(c.rd, hdr[:]); err != nil {
			return nil, errWSClosed
		}
		fin, opcode := hdr[0]&0x80 != 0, hdr[0]&0x0F
		masked, n := hdr[1]&0x80 != 0, uint64(hdr[1]&0x7F)
		if !masked {
			return nil, errWSClosed // clients must mask
		}
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return nil, errWSClosed
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.rd, b[:]); err != nil {
				return nil, errWSClosed
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		if n > wsMaxFrame || uint64(len(msg))+n > wsMaxFrame {
			return nil, errWSClosed
		}
		var mask [4]byte
		if _, err := io.ReadFull(c.rd, mask[:]); err != nil {
			return nil, errWSClosed
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(c.rd, data); err != nil {
			return nil, errWSClosed
		}
		for i := range data {
			data[i] ^= mask[i&3]
		}
		switch opcode {
		case 0x8: // close
			c.enqueue(encodeFrame(0x8, nil))
			return nil, errWSClosed
		case 0x9: // ping
			c.enqueue(encodeFrame(0xA, data))
			continue
		case 0xA: // pong
			continue
		case 0x1, 0x2:
			msg, started = data, true
		case 0x0:
			if !started {
				return nil, errWSClosed
			}
			msg = append(msg, data...)
		default:
			return nil, errWSClosed
		}
		if fin {
			return msg, nil
		}
	}
}
