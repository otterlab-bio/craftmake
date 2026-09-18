package colab

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// wsControlClose is the WebSocket close frame opcode.
const wsControlClose = 0x8

// minimalWSConn is a minimal RFC 6455 client connection that supports text
// and binary frames (opcodes 1 and 2) and the close control frame. It is
// built only on the Go standard library so it stays offline-testable.
type minimalWSConn struct {
	conn   net.Conn
	reader *bufio.Reader
}

// DialWebSocket performs the RFC 6455 client handshake over an existing TCP
// connection using a minimal manual implementation.
func DialWebSocket(ctx context.Context, rawURL string, client *http.Client) (*minimalWSConn, error) {
	return DialWebSocketWithHeaders(ctx, rawURL, nil, client)
}

// DialWebSocketWithHeaders performs the RFC 6455 client handshake with extra HTTP headers
// and transparent TLS for wss:// schemes.
func DialWebSocketWithHeaders(ctx context.Context, rawURL string, headers map[string]string, client *http.Client) (*minimalWSConn, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse websocket url: %w", err)
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, fmt.Errorf("unsupported websocket scheme %q", parsed.Scheme)
	}
	host := parsed.Host
	if parsed.Scheme == "wss" && parsed.Port() == "" {
		host = parsed.Host + ":443"
	}
	if parsed.Scheme == "ws" && parsed.Port() == "" {
		host = parsed.Host + ":80"
	}
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("dial websocket: %w", err)
	}
	if parsed.Scheme == "wss" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: parsed.Hostname(),
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		conn = tlsConn
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := parsed.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", path, host, key)
	for k, v := range headers {
		req += fmt.Sprintf("%s: %s\r\n", k, v)
	}
	req += "\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("websocket handshake response: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, &RemoteError{Kind: ClassifyHTTPStatus(resp.StatusCode), Operation: "websocket handshake", StatusCode: resp.StatusCode, Err: fmt.Errorf("handshake failed: %s", resp.Status)}
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		conn.Close()
		return nil, errors.New("missing websocket upgrade header")
	}
	accept := resp.Header.Get("Sec-WebSocket-Accept")
	expected := websocketAccept(key)
	if accept != expected {
		conn.Close()
		return nil, fmt.Errorf("websocket key mismatch")
	}
	_ = conn.SetDeadline(time.Time{})
	return &minimalWSConn{conn: conn, reader: br}, nil
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// WriteText writes a single text frame with the given payload.
func (c *minimalWSConn) WriteText(payload []byte) error {
	if err := c.writeFrame(0x1, payload); err != nil {
		return err
	}
	return nil
}

func (c *minimalWSConn) writeFrame(opcode byte, payload []byte) error {
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	var header []byte
	if len(payload) < 126 {
		header = []byte{0x80 | opcode, byte(len(payload)) | 0x80}
	} else if len(payload) <= 65535 {
		header = []byte{0x80 | opcode, 126 | 0x80, 0, 0}
		binary.BigEndian.PutUint16(header[2:4], uint16(len(payload)))
	} else {
		header = []byte{0x80 | opcode, 127 | 0x80, 0, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint64(header[2:10], uint64(len(payload)))
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	if _, err := c.conn.Write(mask); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_, err := c.conn.Write(masked)
	return err
}

// ReadText reads one server text frame. It reads and ignores empty/ping and
// returns a non-nil error (with an ok=false) only on a close frame or
// protocol error.
func (c *minimalWSConn) ReadText() (string, error) {
	for {
		opcode, payload, fin, err := c.readFrame()
		if err != nil {
			return "", err
		}
		if opcode == wsControlClose {
			return "", fmt.Errorf("websocket closed by peer")
		}
		if opcode == 0x9 { // ping
			continue
		}
		if opcode == 0xa { // pong
			continue
		}
		if opcode == 0x1 && fin { // text
			return string(payload), nil
		}
		// continuation/binary: return text payload
		if opcode == 0x2 {
			return string(payload), nil
		}
	}
}

func (c *minimalWSConn) readFrame() (opcode byte, payload []byte, fin bool, err error) {
	var header [2]byte
	if _, err := io.ReadFull(c.reader, header[:]); err != nil {
		return 0, nil, false, err
	}
	fin = header[0]&0x80 != 0
	opcode = header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
			return 0, nil, false, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(c.reader, ext[:]); err != nil {
			return 0, nil, false, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.reader, mask[:]); err != nil {
			return 0, nil, false, err
		}
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return 0, nil, false, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, fin, nil
}

// Close sends a standard close frame and closes the underlying connection.
func (c *minimalWSConn) Close() error {
	_ = c.writeFrame(wsControlClose, []byte{0x03, 0xe8})
	return c.conn.Close()
}

// Interrupt sends a no-op (a close frame is used as the interrupt signal by
// Jupyter kernels).
func (c *minimalWSConn) Interrupt() error {
	_, err := c.conn.Write([]byte{0x88, 0x80, 0, 0, 0, 0})
	return err
}

var _ = websocketAccept
