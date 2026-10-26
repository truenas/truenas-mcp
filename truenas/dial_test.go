package truenas

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestConnectRecoversFromDialPanic(t *testing.T) {
	orig := wsDial
	wsDial = func(*websocket.Dialer, string) (*websocket.Conn, *http.Response, error) {
		panic("injected dial panic")
	}
	t.Cleanup(func() { wsDial = orig })

	for _, tc := range []struct {
		name  string
		proxy string
	}{
		{"direct", "none"},
		{"proxied", "http://proxyuser:proxysecret@proxy.invalid:3128"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClientWithOptions("wss://truenas.invalid", testAPIKey, &tls.Config{}, Options{APIUser: testAPIUser, Proxy: tc.proxy})
			if err != nil {
				t.Fatalf("NewClientWithOptions: %v", err)
			}

			c.connMu.Lock()
			err = c.connect()
			conn := c.conn
			c.connMu.Unlock()

			if err == nil {
				t.Fatal("connect succeeded despite a panicking dial")
			}
			if !strings.Contains(err.Error(), "websocket dial failed unexpectedly: injected dial panic") {
				t.Errorf("unexpected error: %v", err)
			}
			for _, secret := range []string{testAPIKey, "proxysecret", "proxyuser"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error contains %q: %v", secret, err)
				}
			}
			if conn != nil {
				t.Error("connection set after a failed dial")
			}
		})
	}
}
