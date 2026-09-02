// Package config reads samo-proxy's settings from the environment.
//
// Every knob has a default that is correct for the deployment this proxy was
// built for: cloudflared and samo-proxy on one box outside the VPN, samo-server
// on another box on the same LAN. Nothing here needs to be set for that case
// except the origin address.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// Addr is the listen address. Loopback by default, because the whole
	// CF-Connecting-IP trust model below assumes cloudflared is the only thing
	// that can reach us. See TrustedCIDRs.
	Addr string

	// Origin is the samo-server base URL on the LAN, e.g. http://192.168.1.10:6969.
	Origin *url.URL

	// ForwardedProto is what to tell samo-server the client's scheme was.
	// cloudflared terminates TLS at Cloudflare's edge and forwards plain HTTP,
	// so the origin cannot work this out for itself — and it needs to, because
	// publicURL() builds absolute URLs from it and security_headers.go gates
	// HSTS on it.
	ForwardedProto string

	// TrustedCIDRs are the source addresses whose CF-Connecting-IP and
	// X-Forwarded-For headers may be believed.
	//
	// This is the security-critical setting in this file. Cloudflare's edge
	// overwrites CF-Connecting-IP on every request, so the value cloudflared
	// hands us is trustworthy — but only because it came from cloudflared. Any
	// other client that can reach this port can put whatever it likes in that
	// header, and samo-server's login limiter (internal/api/login_limiter.go)
	// keys its per-IP brute-force lockout on exactly that header. An untrusted
	// source that we forward unmodified gets to rotate the lockout key at will.
	TrustedCIDRs []*net.IPNet

	// Compression.
	CompressMinBytes int

	// ImageDefaultWidth is injected as `?width=` on artwork requests that do
	// not carry one.
	//
	// samo-server's thumbnail ladder (internal/images/thumbnail.go) has been
	// there all along, but only the desktop client ever asks for a rung — the
	// Android client builds /media/images/{id}/image bare and gets the full
	// embedded cover for every grid tile. Injecting a default here fixes that
	// for every client at once without touching either of them, and it is safe
	// because the origin treats the parameter as advisory: an unresizable
	// source falls through to the original bytes rather than failing.
	//
	// 768 is a real rung on that ladder and is chosen over 512 so a phone's
	// full-screen player art still looks right. Zero disables injection.
	ImageDefaultWidth int

	// Transcode settings.
	TranscodeEnabled bool
	// TranscodeLossyToo re-encodes lossy sources above the bitrate cap as well
	// as lossless ones. Off by default: re-encoding lossy audio is a real
	// quality loss for a much smaller saving than FLAC -> Opus buys.
	TranscodeLossyToo bool
	TranscodeCodec    string
	TranscodeBitrate  int
	FFmpegPath        string
	FFmpegTimeout     time.Duration

	// CacheDir holds transcoded audio. Empty disables the cache, which also
	// disables transcoding — an uncached transcode would re-run ffmpeg on every
	// seek and every replay.
	CacheDir      string
	CacheMaxBytes int64

	// OriginTimeouts.
	OriginDialTimeout time.Duration
	// OriginIdleConns is deliberately generous. The origin is on the LAN, so
	// keep-alive reuse is free and safe here — this is not the environment that
	// forced the client's no-reuse pool (see samo's SamoHttp.kt).
	OriginIdleConns int

	LogLevel string
}

const (
	defaultAddr              = "127.0.0.1:6767"
	defaultOrigin            = "http://192.168.1.10:6969"
	defaultCompressMinBytes  = 1024
	defaultImageWidth        = 768
	defaultTranscodeCodec    = "opus"
	defaultTranscodeBitrate  = 128
	defaultCacheMaxBytes     = 16 << 30 // 16 GiB
	defaultFFmpegTimeout     = 10 * time.Minute
	defaultOriginDialTimeout = 5 * time.Second
	defaultOriginIdleConns   = 64
)

// Load resolves configuration from the environment, returning an error rather
// than a partly-valid Config: a proxy that starts with a bad origin or an
// unparseable trust list is worse than one that refuses to start.
func Load() (*Config, error) {
	cfg := &Config{
		Addr:              env("SAMOPROXY_ADDR", defaultAddr),
		ForwardedProto:    env("SAMOPROXY_FORWARDED_PROTO", "https"),
		CompressMinBytes:  envInt("SAMOPROXY_COMPRESS_MIN_BYTES", defaultCompressMinBytes),
		ImageDefaultWidth: envInt("SAMOPROXY_IMAGE_DEFAULT_WIDTH", defaultImageWidth),
		TranscodeEnabled:  envBool("SAMOPROXY_TRANSCODE", true),
		TranscodeLossyToo: envBool("SAMOPROXY_TRANSCODE_LOSSY", false),
		TranscodeCodec:    strings.ToLower(env("SAMOPROXY_TRANSCODE_CODEC", defaultTranscodeCodec)),
		TranscodeBitrate:  envInt("SAMOPROXY_TRANSCODE_BITRATE", defaultTranscodeBitrate),
		FFmpegPath:        env("SAMOPROXY_FFMPEG", "ffmpeg"),
		FFmpegTimeout:     envDuration("SAMOPROXY_FFMPEG_TIMEOUT", defaultFFmpegTimeout),
		CacheDir:          env("SAMOPROXY_CACHE_DIR", "/var/cache/samo-proxy"),
		CacheMaxBytes:     int64(envInt("SAMOPROXY_CACHE_MAX_MB", defaultCacheMaxBytes>>20)) << 20,
		OriginDialTimeout: envDuration("SAMOPROXY_ORIGIN_DIAL_TIMEOUT", defaultOriginDialTimeout),
		OriginIdleConns:   envInt("SAMOPROXY_ORIGIN_IDLE_CONNS", defaultOriginIdleConns),
		LogLevel:          strings.ToLower(env("SAMOPROXY_LOG_LEVEL", "info")),
	}

	origin, err := parseOrigin(env("SAMOPROXY_ORIGIN", defaultOrigin))
	if err != nil {
		return nil, err
	}
	cfg.Origin = origin

	trusted, err := parseCIDRs(env("SAMOPROXY_TRUST_FORWARDED_FROM", "127.0.0.0/8,::1/128"))
	if err != nil {
		return nil, err
	}
	cfg.TrustedCIDRs = trusted

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.ForwardedProto != "http" && c.ForwardedProto != "https" {
		return fmt.Errorf("SAMOPROXY_FORWARDED_PROTO must be http or https, got %q", c.ForwardedProto)
	}
	switch c.TranscodeCodec {
	case "opus", "aac", "mp3":
	default:
		return fmt.Errorf("SAMOPROXY_TRANSCODE_CODEC must be opus, aac or mp3, got %q", c.TranscodeCodec)
	}
	if c.TranscodeBitrate < 32 || c.TranscodeBitrate > 512 {
		return fmt.Errorf("SAMOPROXY_TRANSCODE_BITRATE must be between 32 and 512, got %d", c.TranscodeBitrate)
	}
	// A width that is not on samo-server's ladder is not an error there — the
	// origin snaps it up to the next rung — so it is not an error here either.
	// A negative one is a typo worth catching.
	if c.ImageDefaultWidth < 0 {
		return fmt.Errorf("SAMOPROXY_IMAGE_DEFAULT_WIDTH cannot be negative, got %d", c.ImageDefaultWidth)
	}
	// Transcoding without somewhere to put the result would re-run ffmpeg for
	// every seek and every replay of the same track. Refuse the combination
	// rather than silently thrashing the CPU.
	if c.TranscodeEnabled && strings.TrimSpace(c.CacheDir) == "" {
		return fmt.Errorf("SAMOPROXY_TRANSCODE is on but SAMOPROXY_CACHE_DIR is empty")
	}
	return nil
}

// TrustsAddr reports whether headers from this remote address may be believed.
func (c *Config) TrustsAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	for _, cidr := range c.TrustedCIDRs {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func parseOrigin(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("SAMOPROXY_ORIGIN is empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("SAMOPROXY_ORIGIN %q: %w", raw, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("SAMOPROXY_ORIGIN %q: scheme must be http or https", raw)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("SAMOPROXY_ORIGIN %q: no host", raw)
	}
	// Trailing slashes would double up when joined with a request path.
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed, nil
}

func parseCIDRs(raw string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// A bare address is a perfectly reasonable thing to write; treat it as
		// a single-host network rather than rejecting it.
		if !strings.Contains(part, "/") {
			ip := net.ParseIP(part)
			if ip == nil {
				return nil, fmt.Errorf("SAMOPROXY_TRUST_FORWARDED_FROM: %q is not an address or CIDR", part)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("SAMOPROXY_TRUST_FORWARDED_FROM: %q: %w", part, err)
		}
		out = append(out, network)
	}
	return out, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
