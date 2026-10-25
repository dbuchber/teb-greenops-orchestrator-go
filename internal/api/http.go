// Package api spricht mit den drei Endpunkten eines Laufs: Cloud (egoscale), Work-Status und Grid.
// Alle Clients teilen sich einen Transport mit Keep-Alive, damit kein Aufruf einen neuen TLS-Handshake kostet.
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

// Transport ist der gemeinsame HTTP-Transport (Keep-Alive, HTTP/2, genug Leerlaufverbindungen je Host).
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

// Account ist ein Eintrag aus der heruntergeladenen CLI-Konfiguration bzw. aus den Umgebungsvariablen.
type Account struct{ Name, Key, Secret, Endpoint string }

// signEXO2 setzt den EXO2-HMAC-SHA256-Header für einen GET ohne Body und Query (Work-Status).
func signEXO2(req *http.Request, key, secret string) {
	expiry := strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.Join([]string{req.Method + " " + req.URL.EscapedPath(), "", "", "", expiry}, "\n")))
	req.Header.Set("Authorization", "EXO2-HMAC-SHA256 credential="+key+",expires="+expiry+
		",signature="+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// CheckEndpoint lässt nur Challenge-Endpunkte /runs/{id}/cloud[/zone]/v2 zu, nie die echte Exoscale-Cloud.
func CheckEndpoint(raw, reference string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" ||
		(len(parts) != 4 && len(parts) != 5) || parts[0] != "runs" || parts[2] != "cloud" || parts[len(parts)-1] != "v2" {
		return fmt.Errorf("kein Challenge-Endpunkt /runs/{id}/cloud[/zone]/v2: %s", raw)
	}
	if u.Hostname() == "exoscale.com" || strings.HasSuffix(u.Hostname(), ".exoscale.com") {
		return fmt.Errorf("echte Exoscale-Cloud ist nicht erlaubt")
	}
	if reference != "" {
		r, _ := url.Parse(reference)
		if u.Scheme != r.Scheme || u.Host != r.Host || parts[1] != strings.Split(strings.Trim(r.Path, "/"), "/")[1] {
			return fmt.Errorf("Endpunkt verlässt den konfigurierten Lauf")
		}
	}
	return nil
}

// RateLimited meldet HTTP 429; der Aufrufer wartet kurz und versucht es erneut.
type RateLimited struct{}

func (RateLimited) Error() string { return "rate limited (429)" }
