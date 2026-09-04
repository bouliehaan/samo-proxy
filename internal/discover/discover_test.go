package discover

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"
)

func TestBroadcastAddr(t *testing.T) {
	cases := []struct {
		name string
		cidr string
		want string
	}{
		{"a /24", "192.168.1.10/24", "192.168.1.255"},
		{"a /16", "172.22.0.5/16", "172.22.255.255"},
		{"a /30", "10.0.0.1/30", "10.0.0.3"},
		{"a single host", "10.0.0.7/32", "10.0.0.7"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ip, network, err := net.ParseCIDR(tc.cidr)
			if err != nil {
				t.Fatalf("parse %s: %v", tc.cidr, err)
			}
			network.IP = ip
			got := broadcastAddr(network)
			if got == nil || got.String() != tc.want {
				t.Fatalf("broadcastAddr(%s) = %v, want %s", tc.cidr, got, tc.want)
			}
		})
	}
}

// An IPv6 network has no broadcast address at all, and asking for one must not
// produce a bogus target to send probes to.
func TestBroadcastAddrRejectsIPv6(t *testing.T) {
	_, network, err := net.ParseCIDR("fd00::1/64")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := broadcastAddr(network); got != nil {
		t.Fatalf("broadcastAddr(v6) = %v, want nil", got)
	}
}

// The global broadcast address is always a target, whatever the interfaces look
// like — a host with none up still has to try.
func TestBroadcastTargetsAlwaysIncludesGlobal(t *testing.T) {
	targets := broadcastTargets()
	if len(targets) == 0 {
		t.Fatal("no targets at all")
	}
	if !targets[0].IP.Equal(net.IPv4bcast) || targets[0].Port != discoveryPort {
		t.Fatalf("first target = %v, want 255.255.255.255:%d", targets[0], discoveryPort)
	}
	for _, target := range targets {
		if target.Port != discoveryPort {
			t.Fatalf("target %v is not on the discovery port", target)
		}
	}
}

// Find must respect its timeout whatever the network looks like. Both outcomes
// are legitimate — a developer machine on the same LAN as a samo-server really
// will get an answer — so this asserts the bound and the shape of each, rather
// than assuming the network is empty.
func TestFindRespectsItsTimeout(t *testing.T) {
	start := time.Now()
	server, err := Find(context.Background(), 150*time.Millisecond)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("took %s to return on a 150ms timeout", elapsed)
	}
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		return
	}
	// A server answered: whatever it said has to be usable as an origin, since
	// that is exactly what the caller does with it.
	parsed, parseErr := url.Parse(server.Address)
	if parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
		t.Fatalf("advertised address %q is not a usable origin", server.Address)
	}
}

// A cancelled context has to unblock the read rather than making the caller
// wait out the full timeout — that is the difference between a fast shutdown
// and a three-second one.
func TestFindHonoursContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Find(ctx, 30*time.Second)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Find ignored the cancelled context")
	}
}

// The reply is JSON on the wire, and a garbled or truncated one must not be
// handed on as an origin.
func TestServerDecodesTheBroadcasterReply(t *testing.T) {
	// Byte for byte what internal/discovery/broadcaster.go marshals.
	raw := `{"Address":"http://192.168.1.10:6969","Id":"srv-abc","Name":"Samo Server"}`
	var server Server
	if err := json.Unmarshal([]byte(raw), &server); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if server.Address != "http://192.168.1.10:6969" {
		t.Fatalf("Address = %q", server.Address)
	}
	if server.ID != "srv-abc" {
		t.Fatalf("ID = %q — the wire field is Id, not ID", server.ID)
	}
	if server.Name != "Samo Server" {
		t.Fatalf("Name = %q", server.Name)
	}
}
