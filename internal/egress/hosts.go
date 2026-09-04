package egress

import (
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"syscall"
)

// HostSet is the closed allow-list of names this proxy will connect to.
//
// Entries are either an exact host (cdn-images.dzcdn.net) or a suffix written
// with a leading dot (.dzcdn.net), which matches that domain and anything under
// it. A suffix entry never matches a name that merely ends in the same letters:
// "evil-dzcdn.net" is not under ".dzcdn.net", which is the whole reason the
// leading dot is required rather than doing a bare strings.HasSuffix.
type HostSet struct {
	exact    map[string]struct{}
	suffixes []string
}

// DefaultAllowHosts is the list this was built for: Deezer's image CDN and its
// aliases, which are the only hosts observed to refuse the VPN exit address
// while their API happily serves it.
//
// This is deliberately not "everything Deezer" — api.deezer.com works fine over
// the VPN and belongs there, where the lookup traffic is covered. Only the
// download hop moves.
var DefaultAllowHosts = []string{
	"cdn-images.dzcdn.net",
	"e-cdns-images.dzcdn.net",
	"cdns-images.dzcdn.net",
}

// ParseHostSet builds a HostSet from a comma-separated list.
func ParseHostSet(raw string) (*HostSet, error) {
	set := &HostSet{exact: map[string]struct{}{}}
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(part)), ".")
		if entry == "" {
			continue
		}
		if strings.ContainsAny(entry, "/:") {
			return nil, fmt.Errorf("egress allow-list: %q looks like a URL, not a host", entry)
		}
		if entry == "." || entry == "*" {
			return nil, fmt.Errorf("egress allow-list: %q would allow everything", entry)
		}
		if strings.HasPrefix(entry, "*.") {
			// Accept the shape people habitually write and store it as a
			// suffix, so both spellings mean the same thing.
			entry = entry[1:]
		}
		if strings.HasPrefix(entry, ".") {
			set.suffixes = append(set.suffixes, entry)
			continue
		}
		set.exact[entry] = struct{}{}
	}
	if set.Len() == 0 {
		return nil, fmt.Errorf("egress allow-list is empty")
	}
	sort.Strings(set.suffixes)
	return set, nil
}

// Contains reports whether host may be connected to.
func (s *HostSet) Contains(host string) bool {
	if s == nil {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return false
	}
	if _, ok := s.exact[host]; ok {
		return true
	}
	for _, suffix := range s.suffixes {
		// suffix carries its leading dot, so this cannot match a sibling name
		// that happens to share the trailing characters.
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// Len is the number of entries, used to reject an empty list at startup.
func (s *HostSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.exact) + len(s.suffixes)
}

// String renders the list for the startup log, so what is actually reachable is
// visible without reading the environment back.
func (s *HostSet) String() string {
	if s == nil {
		return ""
	}
	names := make([]string, 0, s.Len())
	for host := range s.exact {
		names = append(names, host)
	}
	names = append(names, s.suffixes...)
	sort.Strings(names)
	return strings.Join(names, ",")
}

// denyPrivateDestinations refuses a connection whose *resolved* address is on a
// private, loopback or link-local network.
//
// The allow-list is a list of names, and a name is resolved by DNS, which this
// process does not control. Without this check an allow-listed name that
// resolved — through a poisoned cache, a hostile resolver, or simply a
// typo-squatted record — to 192.168.1.x would turn the egress port into a way
// to reach the other services on this box: immich and nextcloud are both on
// this LAN. Checking after resolution rather than before is what closes the
// rebinding window, so this runs as the dialer's Control hook, which sees the
// address actually about to be connected to.
func denyPrivateDestinations(network, address string, _ syscall.RawConn) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return fmt.Errorf("egress: refusing non-tcp network %q", network)
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("egress: unparseable dial address %q", address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("egress: unparseable dial address %q", address)
	}
	if addr.Is4In6() {
		addr = addr.Unmap()
	}
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("egress: refusing to connect to non-public address %s", addr)
	}
	// 100.64.0.0/10, the carrier-grade NAT range, is also where Tailscale
	// lives, so it is private in every sense that matters here even though
	// netip does not class it as such.
	if addr.Is4() && addr.As4()[0] == 100 && addr.As4()[1]&0xc0 == 64 {
		return fmt.Errorf("egress: refusing to connect to CGNAT address %s", addr)
	}
	return nil
}

// parseBasic pulls the credential out of a Basic authorization header.
func parseBasic(header string) (user, password string, ok bool) {
	const prefix = "basic "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", "", false
	}
	user, password, found := strings.Cut(string(decoded), ":")
	if !found {
		return "", "", false
	}
	return user, password, true
}
