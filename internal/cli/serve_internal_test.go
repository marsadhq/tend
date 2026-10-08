package cli

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// TestHTTPServerDropsStalledRequestBody proves the read timeout covers the
// request body, not only the headers. The client sends complete headers that
// promise a body and then goes silent; the handler blocked on that body must
// be released once the timeout passes instead of waiting forever.
func TestHTTPServerDropsStalledRequestBody(t *testing.T) {
	released := make(chan error, 1)
	srv := newHTTPServer("", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		released <- err
	}), 200*time.Millisecond)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprint(conn, "POST /login HTTP/1.1\r\nHost: tend\r\n"+
		"Content-Type: application/x-www-form-urlencoded\r\nContent-Length: 64\r\n\r\n"); err != nil {
		t.Fatalf("write headers: %v", err)
	}

	select {
	case err := <-released:
		if err == nil {
			t.Fatal("handler read a complete body, but none was sent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler still blocked on the request body 5s after a 200ms read timeout")
	}
}
