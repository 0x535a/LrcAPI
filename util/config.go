package util

import (
	"context"
	"net"
	"net/http"
	"time"
)

var (
	GlobalHTTPClient *http.Client
	UA               string = `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/129.0.0.0 Safari/537.36 Edg/129.0.0.0`
)

func NewIPv4Transport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			d := &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 60 * time.Second,
			}
			return d.DialContext(ctx, "tcp4", addr)
		},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

func init() {
	GlobalHTTPClient = &http.Client{
		Transport: NewIPv4Transport(),
		Timeout:   30 * time.Second,
	}
}
