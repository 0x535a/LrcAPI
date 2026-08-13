package util

import (
	"net/http"
	"time"
)

var httpRetryDelay = 200 * time.Millisecond

var HTTPDo = doHTTPWithRetry

func doHTTPWithRetry(req *http.Request) (*http.Response, error) {
	resp, err := GlobalHTTPClient.Do(req)
	if err == nil {
		return resp, nil
	}
	if httpRetryDelay > 0 {
		time.Sleep(httpRetryDelay)
	}
	return GlobalHTTPClient.Do(req.Clone(req.Context()))
}
