package util

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIPv4TransportDialsIPv4(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()
	tr := NewIPv4Transport()
	conn, err := tr.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial ipv4: %v", err)
	}
	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("accept timeout")
	}
}

func TestIPv4TransportRejectsIPv6Literal(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("ipv6 not available")
	}
	defer ln.Close()
	tr := NewIPv4Transport()
	_, err = tr.DialContext(context.Background(), "tcp", ln.Addr().String())
	if err == nil {
		t.Fatal("expected ipv4 dialer to reject [::1]")
	}
}

type failThenOK struct {
	n    int
	next http.RoundTripper
}

func (f *failThenOK) RoundTrip(r *http.Request) (*http.Response, error) {
	f.n++
	if f.n == 1 {
		return nil, errors.New("dial tcp: lookup mobileservice.kugou.com: no such host")
	}
	return f.next.RoundTrip(r)
}

func TestDoHTTPWithRetry(t *testing.T) {
	prevDelay := httpRetryDelay
	httpRetryDelay = 0
	t.Cleanup(func() { httpRetryDelay = prevDelay })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	rt := &failThenOK{next: http.DefaultTransport}
	prevClient := GlobalHTTPClient
	GlobalHTTPClient = &http.Client{Transport: rt, Timeout: 5 * time.Second}
	t.Cleanup(func() { GlobalHTTPClient = prevClient })

	req, err := http.NewRequest("GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doHTTPWithRetry(req)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Fatalf("body=%q", body)
	}
	if rt.n != 2 {
		t.Fatalf("roundtrips=%d want 2", rt.n)
	}
}

func TestDoHTTPNoRetryOnSuccess(t *testing.T) {
	prevDelay := httpRetryDelay
	httpRetryDelay = 0
	t.Cleanup(func() { httpRetryDelay = prevDelay })

	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	req, err := http.NewRequest("GET", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doHTTPWithRetry(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Fatalf("hits=%d want 1", hits)
	}
}
