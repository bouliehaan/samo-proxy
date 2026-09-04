// Command samo-proxy is the internet-facing edge for a samo-server that lives
// behind a VPN.
//
// It exists because of where cloudflared has to run. A samo box with a
// commercial VPN kill-switch sends every outbound packet through the tunnel
// provider, cloudflared included — so the connection Cloudflare's edge sees is
// a tunnel inside a tunnel, with the VPN's congestion and MTU problems on top
// of the real uplink. Running cloudflared on a second box whose default route
// is the plain WAN removes that entirely, and once there is a process sitting
// on that boundary it is the right place to do everything else a self-hosted
// audio service needs when its bytes leave the house: compress what compresses,
// size the artwork, and re-encode lossless audio that has no business crossing
// a home uplink at 4.5 Mbps.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bouliehaan/samo-proxy/internal/cache"
	"github.com/bouliehaan/samo-proxy/internal/compress"
	"github.com/bouliehaan/samo-proxy/internal/config"
	"github.com/bouliehaan/samo-proxy/internal/egress"
	"github.com/bouliehaan/samo-proxy/internal/forward"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// No logger yet, and a config error is the one thing that must be
		// legible before anything else is set up.
		os.Stderr.WriteString("samo-proxy: " + err.Error() + "\n")
		os.Exit(1)
	}

	logger := newLogger(cfg.LogLevel)

	store, err := cache.New(cfg.CacheDir, cfg.CacheMaxBytes)
	if err != nil {
		logger.Error("cache unavailable", "error", err)
		os.Exit(1)
	}

	handler := forward.New(cfg, store, logger)

	mux := http.NewServeMux()
	// samo-proxy's own probe, distinct from samo-server's /health, which is
	// proxied through like any other route. This one answers "is the proxy up
	// and can it see the origin", which is what a container healthcheck and a
	// systemd watchdog actually want to know.
	mux.HandleFunc("GET /_samoproxy/health", func(w http.ResponseWriter, r *http.Request) {
		if err := handler.Health(r.Context()); err != nil {
			logger.Warn("health check failed", "error", err)
			http.Error(w, "origin unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.Handle("/", handler)

	server := &http.Server{
		Addr: cfg.Addr,
		// Logging outermost so it records the status and byte count that were
		// actually sent, compression included.
		Handler: forward.LogRequests(logger, compress.Middleware(cfg.CompressMinBytes, mux)),
		// Slowloris defence, matching samo-server's own posture.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// ReadTimeout and WriteTimeout stay zero for the same reason they do in
		// samo-server: a WriteTimeout would cut every stream off mid-playback,
		// and an endless radio channel is a legitimately infinite response.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The egress proxy is a second listener rather than another route on the
	// first, and that separation is deliberate. The inbound listener's whole
	// trust model is "only cloudflared can reach this, so its forwarding
	// headers may be believed" (see config.TrustedCIDRs); the egress listener
	// has to be reachable from the LAN for samo-server to use it. Folding the
	// two together would mean widening the first to serve the second, which is
	// precisely the mistake the README warns about.
	egressServer, err := startEgress(cfg, logger, stop)
	if err != nil {
		logger.Error("egress proxy unavailable", "error", err)
		os.Exit(1)
	}

	go func() {
		logger.Info("samo-proxy listening",
			"addr", cfg.Addr,
			"origin", cfg.Origin.String(),
			"originFrom", originSource(cfg),
			"transcode", transcodeSummary(cfg),
			"imageWidth", cfg.ImageDefaultWidth,
			"cacheDir", cfg.CacheDir,
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	// Long enough for an in-flight track to finish being written to a client,
	// short enough that a restart is not a coffee break. An endless radio
	// stream will be cut, which is correct — it would otherwise never end.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("graceful shutdown incomplete", "error", err)
	}
	if egressServer != nil {
		if err := egressServer.Shutdown(shutdownCtx); err != nil {
			logger.Warn("egress shutdown incomplete", "error", err)
		}
	}
}

// startEgress brings up the outbound CONNECT proxy, or returns (nil, nil) when
// it is not configured. A configuration error is fatal rather than a warning:
// somebody who set SAMOPROXY_EGRESS_ADDR wants that port open, and a proxy that
// quietly declined would show up as artist photos still failing days later.
func startEgress(cfg *config.Config, logger *slog.Logger, stop context.CancelFunc) (*http.Server, error) {
	if !cfg.EgressEnabled() {
		return nil, nil
	}

	allow, err := egress.ParseHostSet(cfg.EgressAllowHosts)
	if err != nil {
		return nil, err
	}
	handler, err := egress.New(egress.Options{
		Allow:  allow,
		Token:  cfg.EgressToken,
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}

	server := &http.Server{
		Addr:    cfg.EgressAddr,
		Handler: handler,
		// No ReadTimeout or WriteTimeout: a CONNECT tunnel is hijacked out from
		// under the server and manages its own idle deadlines, and a timeout
		// here would cut a large download in the middle.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelDebug),
	}

	listener, err := net.Listen("tcp", cfg.EgressAddr)
	if err != nil {
		return nil, err
	}

	go func() {
		logger.Info("egress proxy listening",
			"addr", cfg.EgressAddr,
			"allow", allow.String(),
		)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("egress listener failed", "error", err)
			stop()
		}
	}()
	return server, nil
}

// originSource says whether the origin was found on the LAN or configured, so
// a proxy pointed at the wrong server is one log line away from being obvious.
func originSource(cfg *config.Config) string {
	if cfg.OriginDiscovered {
		return "discovery"
	}
	return "SAMOPROXY_ORIGIN"
}

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch level {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	default:
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: parsed}))
}

func transcodeSummary(cfg *config.Config) string {
	if !cfg.TranscodeEnabled {
		return "off"
	}
	summary := cfg.TranscodeCodec + " @ " + itoa(cfg.TranscodeBitrate) + "k (lossless sources"
	if cfg.TranscodeLossyToo {
		summary += " and lossy"
	}
	return summary + ")"
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [8]byte
	i := len(digits)
	for value > 0 {
		i--
		digits[i] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[i:])
}
