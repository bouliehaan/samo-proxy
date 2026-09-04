package egress

import (
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func mustHostSet(t *testing.T, raw string) *HostSet {
	t.Helper()
	set, err := ParseHostSet(raw)
	if err != nil {
		t.Fatalf("ParseHostSet(%q): %v", raw, err)
	}
	return set
}

func TestHostSetExactAndSuffix(t *testing.T) {
	set := mustHostSet(t, "cdn-images.dzcdn.net, .example.org, *.wild.net")

	allowed := []string{
		"cdn-images.dzcdn.net",
		"CDN-Images.DZCDN.net",  // case folded
		"cdn-images.dzcdn.net.", // trailing root dot
		"a.example.org",
		"deep.nested.example.org",
		"x.wild.net",
	}
	for _, host := range allowed {
		if !set.Contains(host) {
			t.Errorf("Contains(%q) = false, want true", host)
		}
	}

	// The leading dot is what stops a suffix entry matching a sibling name that
	// merely ends in the same letters. That is the bug this shape prevents.
	denied := []string{
		"",
		"dzcdn.net",
		"evil-cdn-images.dzcdn.net.attacker.com",
		"notexample.org",
		"example.org.evil.com",
		"wild.net.evil.com",
		"cdn-images.dzcdn.net.evil.com",
	}
	for _, host := range denied {
		if set.Contains(host) {
			t.Errorf("Contains(%q) = true, want false", host)
		}
	}
}

func TestParseHostSetRejectsUnsafeEntries(t *testing.T) {
	for _, raw := range []string{"", "   ", ",,", "*", ".", "https://example.org", "example.org:443"} {
		if _, err := ParseHostSet(raw); err == nil {
			t.Errorf("ParseHostSet(%q) succeeded, want error", raw)
		}
	}
}

func TestNewRequiresTokenAndAllowList(t *testing.T) {
	if _, err := New(Options{Allow: mustHostSet(t, "example.org")}); err == nil {
		t.Error("New with no token succeeded, want error")
	}
	if _, err := New(Options{Token: "secret"}); err == nil {
		t.Error("New with no allow-list succeeded, want error")
	}
}

func TestDenyPrivateDestinations(t *testing.T) {
	// An allow-listed name that resolves into the LAN is the rebinding case:
	// the edge box also runs immich and nextcloud, so this check is what keeps
	// the egress port from reaching them.
	refused := []string{
		"127.0.0.1:443", "10.1.2.3:443", "192.168.1.12:443", "172.16.5.5:443",
		"169.254.1.1:443", "0.0.0.0:443", "[::1]:443", "[fe80::1]:443",
		"100.100.1.1:443", // CGNAT / Tailscale
		"[::ffff:192.168.1.12]:443",
	}
	for _, address := range refused {
		if err := denyPrivateDestinations("tcp", address, nil); err == nil {
			t.Errorf("denyPrivateDestinations(%q) = nil, want refusal", address)
		}
	}
	for _, address := range []string{"93.184.216.34:443", "[2606:2800:220:1::]:443"} {
		if err := denyPrivateDestinations("tcp", address, nil); err != nil {
			t.Errorf("denyPrivateDestinations(%q) = %v, want nil", address, err)
		}
	}
	if err := denyPrivateDestinations("udp", "93.184.216.34:443", nil); err == nil {
		t.Error("non-tcp network accepted, want refusal")
	}
}

func TestSplitTarget(t *testing.T) {
	host, port, err := splitTarget("cdn-images.dzcdn.net:443")
	if err != nil || host != "cdn-images.dzcdn.net" || port != "443" {
		t.Fatalf("splitTarget = (%q,%q,%v)", host, port, err)
	}
	if _, _, err := splitTarget("cdn-images.dzcdn.net"); err == nil {
		t.Error("a bare host was accepted; CONNECT must carry a port")
	}
	if _, _, err := splitTarget(""); err == nil {
		t.Error("an empty target was accepted")
	}
}

// newTestProxy starts the egress handler on a real listener and returns its
// address, so the tests exercise the hijack path rather than a recorder.
func newTestProxy(t *testing.T, allow string) string {
	t.Helper()
	handler, err := New(Options{Allow: mustHostSet(t, allow), Token: "s3cret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func connect(t *testing.T, proxyAddr, target, token string) (int, net.Conn) {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if token != "" {
		credential := base64.StdEncoding.EncodeToString([]byte("samo:" + token))
		request += "Proxy-Authorization: Basic " + credential + "\r\n"
	}
	request += "\r\n"
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("read CONNECT response: %v", err)
	}
	status := string(buf[:n])
	code := 0
	if fields := strings.Fields(status); len(fields) >= 2 {
		for _, r := range fields[1] {
			if r < '0' || r > '9' {
				break
			}
			code = code*10 + int(r-'0')
		}
	}
	return code, conn
}

func TestConnectRequiresToken(t *testing.T) {
	proxy := newTestProxy(t, "cdn-images.dzcdn.net")

	code, conn := connect(t, proxy, "cdn-images.dzcdn.net:443", "")
	conn.Close()
	if code != http.StatusProxyAuthRequired {
		t.Errorf("no credential: status = %d, want 407", code)
	}

	code, conn = connect(t, proxy, "cdn-images.dzcdn.net:443", "wrong")
	conn.Close()
	if code != http.StatusProxyAuthRequired {
		t.Errorf("bad credential: status = %d, want 407", code)
	}
}

func TestConnectRefusesHostsOffTheAllowList(t *testing.T) {
	proxy := newTestProxy(t, "cdn-images.dzcdn.net")

	code, conn := connect(t, proxy, "example.com:443", "s3cret")
	conn.Close()
	if code != http.StatusForbidden {
		t.Errorf("off-list host: status = %d, want 403", code)
	}
}

func TestConnectRefusesNonWebPorts(t *testing.T) {
	proxy := newTestProxy(t, "cdn-images.dzcdn.net")

	// An allow-listed name on a port that is not web traffic is the difference
	// between a web proxy and a general TCP relay.
	code, conn := connect(t, proxy, "cdn-images.dzcdn.net:22", "s3cret")
	conn.Close()
	if code != http.StatusForbidden {
		t.Errorf("ssh port: status = %d, want 403", code)
	}
}

func TestConnectRefusesNonConnectMethods(t *testing.T) {
	handler, err := New(Options{Allow: mustHostSet(t, "example.org"), Token: "s3cret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "http://example.org/thing", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", recorder.Code)
	}
}

// TestConnectTunnelsToAllowedHost is the end-to-end shape: an allow-listed
// origin, reached through the proxy by a real http.Client configured exactly
// the way samo-server configures its own.
func TestConnectTunnelsToAllowedHost(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write([]byte("jpeg-bytes"))
	}))
	defer origin.Close()

	originURL, err := url.Parse(origin.URL)
	if err != nil {
		t.Fatalf("parse origin: %v", err)
	}
	originHost, originPort, _ := net.SplitHostPort(originURL.Host)

	// The origin listens on loopback, which denyPrivateDestinations exists to
	// refuse, so the handler is built here with that hook left off. Loopback is
	// covered directly by TestDenyPrivateDestinations; what is under test here
	// is the tunnel itself.
	allow, err := ParseHostSet(originHost)
	if err != nil {
		t.Fatalf("ParseHostSet: %v", err)
	}
	handler, err := New(Options{Allow: allow, Token: "s3cret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler.dialer.Control = nil
	allowedPortsOriginal := allowedPorts[originPort]
	allowedPorts[originPort] = true
	defer func() {
		if !allowedPortsOriginal {
			delete(allowedPorts, originPort)
		}
	}()

	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	proxyURL, err := url.Parse("http://samo:s3cret@" + strings.TrimPrefix(proxyServer.URL, "http://"))
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}

	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("GET through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "jpeg-bytes" {
		t.Fatalf("through proxy: status=%d body=%q", resp.StatusCode, body)
	}
	if accepted, refused := handler.Stats(); accepted != 1 || refused != 0 {
		t.Errorf("Stats() = (%d, %d), want (1, 0)", accepted, refused)
	}
}
