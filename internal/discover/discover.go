// Package discover finds a samo-server on the LAN.
//
// samo-server has answered a UDP probe with its own address since long before
// this proxy existed — it is how the Android and desktop clients find a server
// without being told one. Using it here means the same thing is true of a
// service: there is no address to look up, type into a file, and get wrong.
//
// It is a broadcast, so it only works from something that can put a packet on
// the LAN: a host-networked container, or a process on the host. A container on
// Docker's default bridge cannot, which is the same constraint that makes
// samo-server itself run with host networking.
package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

const (
	// probe is the payload samo-server's broadcaster matches, byte for byte.
	probe = "Who is SamoServer?"

	// discoveryPort is where it listens. Deliberately not Jellyfin's.
	discoveryPort = 7360

	// maxDatagram bounds a single read. The reply is a short JSON object;
	// anything larger is not one.
	maxDatagram = 1024
)

// Server is what a samo-server advertises about itself.
type Server struct {
	// Address is an absolute base URL, e.g. http://192.168.1.10:6969. The
	// server fills in the interface address it would reach the prober on, so
	// this is usable from wherever the probe was sent.
	Address string `json:"Address"`
	// ID is stable across restarts, so a caller that already knows a server can
	// tell whether this is the same one.
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// ErrNotFound means nothing answered inside the timeout. It is not necessarily
// an error worth failing on: a caller with a configured address should prefer
// that, and one without it should say so plainly rather than retry forever.
var ErrNotFound = errors.New("no samo-server answered on the LAN")

// Find broadcasts a probe and returns the first server to answer.
//
// The probe goes to the global broadcast address and to each interface's own,
// because a host with several networks may drop the former; duplicates cost
// nothing, since samo-server rate-limits to one reply per source per second and
// the first answer wins either way.
func Find(ctx context.Context, timeout time.Duration) (Server, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return Server{}, fmt.Errorf("open discovery socket: %w", err)
	}
	// Explicitly discarded: this socket is read-only and about to go out of
	// scope, so a close error tells the caller nothing they can act on.
	defer func() { _ = conn.Close() }()

	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return Server{}, fmt.Errorf("set discovery deadline: %w", err)
	}

	// Unblock the read when the caller gives up, rather than making shutdown
	// wait out the full timeout.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-stop:
		}
	}()

	sent := 0
	for _, target := range broadcastTargets() {
		if _, err := conn.WriteToUDP([]byte(probe), target); err == nil {
			sent++
		}
	}
	if sent == 0 {
		return Server{}, fmt.Errorf("%w: no interface accepted a broadcast", ErrNotFound)
	}

	buffer := make([]byte, maxDatagram)
	for {
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return Server{}, ctx.Err()
			}
			return Server{}, ErrNotFound
		}

		var server Server
		if err := json.Unmarshal(buffer[:n], &server); err != nil {
			// Something else on 7360, or a truncated datagram. Keep reading
			// until the deadline rather than failing the whole probe on it.
			continue
		}
		// An advertised address that will not parse is worse than none: it
		// would be handed straight to an http.Client as an origin.
		parsed, err := url.Parse(server.Address)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			continue
		}
		return server, nil
	}
}

// broadcastTargets is the global broadcast address plus each interface's own.
//
// 255.255.255.255 is not always enough: a host with several interfaces may
// route it out only one of them, and some stacks refuse it entirely. Adding the
// per-interface addresses covers both without needing to know the topology.
func broadcastTargets() []*net.UDPAddr {
	targets := []*net.UDPAddr{{IP: net.IPv4bcast, Port: discoveryPort}}

	interfaces, err := net.Interfaces()
	if err != nil {
		return targets
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			network, ok := addr.(*net.IPNet)
			if !ok || network.IP.To4() == nil {
				continue
			}
			if broadcast := broadcastAddr(network); broadcast != nil {
				targets = append(targets, &net.UDPAddr{IP: broadcast, Port: discoveryPort})
			}
		}
	}
	return targets
}

// broadcastAddr is the all-ones host address for a network: the IP with every
// masked-off bit set.
func broadcastAddr(network *net.IPNet) net.IP {
	ip := network.IP.To4()
	mask := net.IP(network.Mask).To4()
	if ip == nil || mask == nil {
		return nil
	}
	broadcast := make(net.IP, net.IPv4len)
	for i := range broadcast {
		broadcast[i] = ip[i] | ^mask[i]
	}
	return broadcast
}
