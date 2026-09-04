// Package egress is the outbound counterpart to the rest of samo-proxy: a
// narrow CONNECT proxy that lets the samo box reach the handful of internet
// hosts that refuse its VPN exit address, without lifting the kill-switch.
//
// # Why this exists
//
// The samo box routes everything through ProtonVPN by design, and that is not
// negotiable — it is the reason the box is safe to run. But commercial VPN exit
// addresses are datacenter addresses, and some CDNs refuse them outright.
// Deezer's image CDN is the case that forced this package: from the samo box,
// api.deezer.com answers normally and returns byte-identical payloads, while
// every single request to cdn-images.dzcdn.net comes back 403. The artist photo
// backfill therefore finds a picture for an artist and then cannot fetch it.
//
// # Why it is deliberately tiny
//
// Anything that leaves the house on the plain WAN rather than the VPN is a
// leak, and this package's whole job is to make that leak as small as it can
// possibly be while still fixing the problem:
//
//   - Only hosts on an explicit allow-list are reachable. The list is not a
//     default-open policy with exceptions; it is closed, and every entry is a
//     host that has been observed to reject the VPN.
//   - Only CONNECT, and only to ports that carry ordinary web traffic. This is
//     not a general TCP relay.
//   - A shared secret is required. The listener has to be reachable from the
//     LAN for samo-server to use it, and anything else on that LAN would
//     otherwise have found an open proxy sitting on the plain WAN.
//   - It is off unless configured. A samo-proxy that nobody has deliberately
//     pointed at this feature does not open the port at all.
//
// The honest tradeoff, stated plainly because the operator deserves to decide
// it rather than discover it: a request to cdn-images.dzcdn.net carries the
// image id of an artist in the library, so an observer on the WAN side learns
// which artists are being looked up, and when. That is strictly more than they
// learned before, when the answer was "nothing". It is a much smaller leak than
// routing the library's whole metadata surface off the VPN, but it is not zero,
// and it is why this defaults to off.
package egress

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Handler proxies CONNECT requests to allow-listed hosts.
type Handler struct {
	allow   *HostSet
	token   string
	dialer  *net.Dialer
	logger  *slog.Logger
	idle    time.Duration
	metrics metrics
}

type metrics struct {
	mu       sync.Mutex
	accepted uint64
	refused  uint64
}

// Options configures a Handler. Token is required; New refuses to build an
// unauthenticated proxy, because the failure mode of forgetting it is an open
// relay on the LAN rather than an error anyone would notice.
type Options struct {
	Allow       *HostSet
	Token       string
	DialTimeout time.Duration
	IdleTimeout time.Duration
	Logger      *slog.Logger
}

// New builds the handler. An empty token or an empty allow-list is an error:
// both are configurations that look like they work and are not what anyone
// meant.
func New(options Options) (*Handler, error) {
	if strings.TrimSpace(options.Token) == "" {
		return nil, errors.New("egress: a token is required")
	}
	if options.Allow == nil || options.Allow.Len() == 0 {
		return nil, errors.New("egress: the allow-list is empty")
	}
	dialTimeout := options.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	idleTimeout := options.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 60 * time.Second
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{
		allow: options.Allow,
		token: options.Token,
		dialer: &net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
			// Enforced on the resolved address, not the requested name — see
			// denyPrivateDestinations for why that ordering is the point.
			Control: denyPrivateDestinations,
		},
		logger: logger,
		idle:   idleTimeout,
	}, nil
}

// allowedPorts is what a CONNECT may target. Restricting this is what keeps the
// package a web proxy rather than a way to reach anything at all on the far
// side of an allow-listed name.
var allowedPorts = map[string]bool{"443": true, "80": true}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// Absolute-URI GET is the other half of the classic proxy protocol, and
		// it is deliberately not implemented: it would mean this process
		// terminating TLS or carrying plaintext for the origin, when CONNECT
		// lets samo-server keep its own end-to-end TLS all the way to the CDN.
		// The proxy never sees the bytes, which is the property worth having.
		h.refuse(w, r, http.StatusMethodNotAllowed, "only CONNECT is supported")
		return
	}

	if !h.authorized(r) {
		// No detail in the body: an unauthenticated caller learns only that it
		// needs credentials, never whether a host would have been allowed.
		w.Header().Set("Proxy-Authenticate", `Basic realm="samo-proxy egress"`)
		h.refuse(w, r, http.StatusProxyAuthRequired, "proxy authentication required")
		return
	}

	host, port, err := splitTarget(r.Host)
	if err != nil {
		h.refuse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !allowedPorts[port] {
		h.refuse(w, r, http.StatusForbidden, "port "+port+" is not permitted")
		return
	}
	if !h.allow.Contains(host) {
		h.refuse(w, r, http.StatusForbidden, "host is not on the egress allow-list")
		return
	}

	upstream, err := h.dialer.DialContext(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		h.logger.Warn("egress dial failed", "host", host, "port", port, "error", err)
		h.refuse(w, r, http.StatusBadGateway, "upstream dial failed")
		return
	}
	defer upstream.Close()

	// Hijack rather than stream: a CONNECT body is not HTTP, so there is
	// nothing for the server's own writer to do but get in the way.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		h.refuse(w, r, http.StatusInternalServerError, "connection cannot be hijacked")
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		h.logger.Error("egress hijack failed", "error", err)
		return
	}
	defer client.Close()

	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	h.count(true)
	h.logger.Debug("egress tunnel open", "host", host, "port", port)

	// Anything the client pipelined behind the CONNECT is already in the
	// buffered reader and would be lost if we read from the raw conn instead.
	if n := buffered.Reader.Buffered(); n > 0 {
		if pending, err := buffered.Reader.Peek(n); err == nil {
			if _, err := upstream.Write(pending); err != nil {
				return
			}
			buffered.Reader.Discard(n)
		}
	}

	h.pipe(client, upstream)
}

// pipe copies in both directions until either side finishes, then unblocks the
// other by closing both. The idle deadline is refreshed on every copied chunk,
// so a slow-but-live transfer is never cut while a genuinely dead tunnel is.
func (h *Handler) pipe(client, upstream net.Conn) {
	var once sync.Once
	shutdown := func() {
		once.Do(func() {
			client.Close()
			upstream.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	copyOneWay := func(dst, src net.Conn) {
		defer wg.Done()
		defer shutdown()
		buf := make([]byte, 32*1024)
		for {
			_ = src.SetReadDeadline(time.Now().Add(h.idle))
			n, readErr := src.Read(buf)
			if n > 0 {
				_ = dst.SetWriteDeadline(time.Now().Add(h.idle))
				if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}
	go copyOneWay(upstream, client)
	go copyOneWay(client, upstream)
	wg.Wait()
}

// authorized checks the shared secret in constant time. The username half of
// the Basic credential is ignored: samo-server has no identity here beyond
// "holds the token", and pretending otherwise would just be a second string to
// keep in sync across two repos.
func (h *Handler) authorized(r *http.Request) bool {
	header := r.Header.Get("Proxy-Authorization")
	if header == "" {
		// Go's own Transport sends the credential on the CONNECT as
		// Proxy-Authorization. Accepting Authorization as well costs nothing
		// and saves an afternoon for anyone testing with a tool that sends it.
		header = r.Header.Get("Authorization")
	}
	_, password, ok := parseBasic(header)
	if !ok {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(password), []byte(h.token)) == 1
}

func (h *Handler) refuse(w http.ResponseWriter, r *http.Request, status int, reason string) {
	h.count(false)
	h.logger.Warn("egress refused",
		"status", status,
		"reason", reason,
		"target", r.Host,
		"from", r.RemoteAddr,
	)
	http.Error(w, reason, status)
}

func (h *Handler) count(accepted bool) {
	h.metrics.mu.Lock()
	defer h.metrics.mu.Unlock()
	if accepted {
		h.metrics.accepted++
		return
	}
	h.metrics.refused++
}

// Stats reports how many tunnels have been opened and refused, for the health
// endpoint. Useful precisely because a misconfigured allow-list is otherwise
// silent: samo-server logs a download failure and the proxy logs nothing at all
// unless someone goes looking.
func (h *Handler) Stats() (accepted, refused uint64) {
	h.metrics.mu.Lock()
	defer h.metrics.mu.Unlock()
	return h.metrics.accepted, h.metrics.refused
}

// splitTarget parses a CONNECT authority. A bare host with no port is rejected
// rather than defaulted: CONNECT is defined to carry one, and guessing would
// mean a typo silently reaching a different service than intended.
func splitTarget(authority string) (host, port string, err error) {
	authority = strings.TrimSpace(authority)
	if authority == "" {
		return "", "", errors.New("no CONNECT target")
	}
	host, port, err = net.SplitHostPort(authority)
	if err != nil {
		return "", "", fmt.Errorf("malformed CONNECT target %q", authority)
	}
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	if host == "" {
		return "", "", fmt.Errorf("malformed CONNECT target %q", authority)
	}
	return host, port, nil
}
