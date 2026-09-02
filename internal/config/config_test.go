package config

import (
	"testing"
)

func TestParseCIDRsAcceptsNetworksAndBareAddresses(t *testing.T) {
	networks, err := parseCIDRs("127.0.0.0/8, ::1/128, 192.168.1.11")
	if err != nil {
		t.Fatalf("parseCIDRs: %v", err)
	}
	if len(networks) != 3 {
		t.Fatalf("parsed %d networks, want 3", len(networks))
	}
	cfg := &Config{TrustedCIDRs: networks}

	trusted := []string{"127.0.0.1:5000", "[::1]:5000", "192.168.1.11:5000"}
	for _, addr := range trusted {
		if !cfg.TrustsAddr(addr) {
			t.Errorf("TrustsAddr(%q) = false, want true", addr)
		}
	}
	untrusted := []string{"203.0.113.9:5000", "192.168.1.12:5000", "10.0.0.1:5000"}
	for _, addr := range untrusted {
		if cfg.TrustsAddr(addr) {
			t.Errorf("TrustsAddr(%q) = true, want false", addr)
		}
	}
}

// A trust list that silently drops a malformed entry would quietly widen or
// narrow the trust boundary. Fail loudly instead.
func TestParseCIDRsRejectsGarbage(t *testing.T) {
	if _, err := parseCIDRs("127.0.0.0/8, not-an-address"); err == nil {
		t.Fatal("parseCIDRs accepted a malformed entry")
	}
}

// An empty trust list means nothing is trusted, which must not degrade into
// trusting everything.
func TestEmptyTrustListTrustsNothing(t *testing.T) {
	cfg := &Config{}
	if cfg.TrustsAddr("127.0.0.1:5000") {
		t.Fatal("an empty trust list trusted a caller")
	}
}

func TestTrustsAddrHandlesAMissingPort(t *testing.T) {
	networks, err := parseCIDRs("127.0.0.0/8")
	if err != nil {
		t.Fatalf("parseCIDRs: %v", err)
	}
	cfg := &Config{TrustedCIDRs: networks}
	if !cfg.TrustsAddr("127.0.0.1") {
		t.Error("a bare address without a port was not matched")
	}
}

func TestParseOriginNormalisesAndValidates(t *testing.T) {
	origin, err := parseOrigin("http://192.168.1.10:6969/")
	if err != nil {
		t.Fatalf("parseOrigin: %v", err)
	}
	if origin.Path != "" {
		t.Errorf("Path = %q, want the trailing slash trimmed", origin.Path)
	}

	for _, bad := range []string{"", "ftp://box", "not a url at all::", "//nohost"} {
		if _, err := parseOrigin(bad); err == nil {
			t.Errorf("parseOrigin(%q) accepted an invalid origin", bad)
		}
	}
}

// Transcoding with nowhere to cache would re-run ffmpeg on every seek and every
// replay.
func TestTranscodeWithoutACacheDirIsRejected(t *testing.T) {
	cfg := &Config{
		ForwardedProto:   "https",
		TranscodeCodec:   "opus",
		TranscodeBitrate: 128,
		TranscodeEnabled: true,
		CacheDir:         "",
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("validate accepted transcoding with no cache directory")
	}
}

func TestValidateRejectsUnknownCodecAndSillyBitrate(t *testing.T) {
	base := func() *Config {
		return &Config{
			ForwardedProto:   "https",
			TranscodeCodec:   "opus",
			TranscodeBitrate: 128,
			CacheDir:         "/tmp",
		}
	}

	bad := base()
	bad.TranscodeCodec = "vorbis"
	if err := bad.validate(); err == nil {
		t.Error("validate accepted an unsupported codec")
	}

	bad = base()
	bad.TranscodeBitrate = 4
	if err := bad.validate(); err == nil {
		t.Error("validate accepted an absurd bitrate")
	}

	bad = base()
	bad.ForwardedProto = "gopher"
	if err := bad.validate(); err == nil {
		t.Error("validate accepted a nonsense forwarded proto")
	}
}
