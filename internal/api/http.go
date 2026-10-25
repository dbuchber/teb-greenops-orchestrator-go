// Package api talks to the three endpoints of a run: cloud (egoscale), work status and grid.
// All clients share one keep-alive transport so no call pays for a new TLS handshake.
package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Transport is the shared HTTP transport (keep-alive, HTTP/2, enough idle connections per host).
var Transport = &http.Transport{
	Proxy:               http.ProxyFromEnvironment,
	DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 32,
	IdleConnTimeout:     90 * time.Second,
	TLSHandshakeTimeout: 5 * time.Second,
}

func newHTTP(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Account is an entry from the downloaded CLI configuration or from the environment variables.
type Account struct{ Name, Key, Secret, Endpoint string }

// signEXO2 sets the EXO2-HMAC-SHA256 header for a GET without body and query (work status).
func signEXO2(req *http.Request, key, secret string) {
	expiry := strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join([]string{req.Method + " " + req.URL.EscapedPath(), "", "", "", expiry}, "\n")))
	req.Header.Set("Authorization", "EXO2-HMAC-SHA256 credential="+key+",expires="+expiry+
		",signature="+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// CheckEndpoint only allows challenge endpoints /runs/{id}/cloud[/zone]/v2, never the real Exoscale cloud.
func CheckEndpoint(raw, reference string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" ||
		(len(parts) != 4 && len(parts) != 5) || parts[0] != "runs" || parts[2] != "cloud" || parts[len(parts)-1] != "v2" {
		return fmt.Errorf("not a challenge endpoint /runs/{id}/cloud[/zone]/v2: %s", raw)
	}
	if u.Hostname() == "exoscale.com" || strings.HasSuffix(u.Hostname(), ".exoscale.com") {
		return fmt.Errorf("the real Exoscale cloud is not allowed")
	}
	if reference != "" {
		r, _ := url.Parse(reference)
		if u.Scheme != r.Scheme || u.Host != r.Host || parts[1] != strings.Split(strings.Trim(r.Path, "/"), "/")[1] {
			return fmt.Errorf("endpoint leaves the configured run")
		}
	}
	return nil
}

// RateLimited reports HTTP 429; the caller waits briefly and retries.
type RateLimited struct{}

func (RateLimited) Error() string { return "rate limited (429)" }
