package colab

import (
	"bufio"
	"net"
	"sync"
	"testing"
)

// TestWebSocketAcceptKey uses the RFC 6455 example vector to verify the
// Sec-WebSocket-Accept derivation.
func TestWebSocketAcceptKey(t *testing.T) {
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got := websocketAccept(key); got != want {
		t.Fatalf("websocketAccept(%q) = %q, want %q", key, got, want)
	}
}

// TestMinimalWebSocketFrameRoundTrip pipes two minimalWSConn ends together
// and checks a text frame survives write+read.
func TestMinimalWebSocketFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	client := &minimalWSConn{conn: a, reader: bufio.NewReader(a)}
	server := &minimalWSConn{conn: b, reader: bufio.NewReader(b)}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _, _, err := server.readFrame()
		if err != nil {
			t.Errorf("readFrame: %v", err)
			return
		}
		_ = server.sendServerText([]byte("hello\nworld"))
	}()

	if err := client.WriteText([]byte("run")); err != nil {
		t.Fatal(err)
	}
	got, err := client.ReadText()
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if got != "hello\nworld" {
		t.Fatalf("ReadText = %q, want %q", got, "hello\nworld")
	}
}

// sendServerText writes an UNMASKED text frame (server frames must not be
// masked per RFC 6455).
func (c *minimalWSConn) sendServerText(payload []byte) error {
	var header []byte
	if len(payload) < 126 {
		header = []byte{0x81, byte(len(payload))}
	} else {
		header = []byte{0x81, 126, 0, byte(len(payload))}
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}
