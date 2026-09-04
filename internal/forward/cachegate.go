package forward

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bouliehaan/samo-proxy/internal/cache"
)

// This file exists because a cache in front of an authenticated origin is an
// authorization bypass unless something stops it being one.
//
// The cache key deliberately excludes every credential — see
// classify.CacheKeyQuery — because a token does not change a single byte of the
// response, and keying on it would give every user their own copy of the same
// album cover. That is the right call for a cache and the wrong one for
// authorization: it means a cache hit is keyed on the URL alone, so anybody who
// knows the URL would be handed the bytes, signed in or not.
//
// Two paths had exactly that hole. serveImage answered every hit straight from
// disk with no origin call at all. serveAudio revalidated a COMPLETE entry —
// which incidentally re-authorized the caller, and is why that path was
// safe — but its Follow branch, which attaches a second listener to an encode
// already in flight, contacted nobody. The first leaked artwork; the second
// leaked audio.
//
// Both were one missing check in one function, which is the shape of bug that
// comes back the next time somebody caches a third kind of thing. So the check
// is not a line to remember, it is the only door: guardedCache is what the
// handler holds instead of a *cache.Cache, and its read methods cannot be
// called without a request to authorize.

// credentialTTL is how long the gate keeps trusting a credential the origin
// accepted.
//
// It is the window in which a revoked token still gets cached bytes, so it
// wants to be short; it is also what keeps a browsing session off the origin,
// so it wants to be long. Five minutes is well inside "the user is still
// looking at the page that authenticated" and well inside any reasonable
// expectation of how fast a revocation takes hold. Only cached responses are
// affected either way — a revoked token stops working on everything else the
// instant the origin says so.
const credentialTTL = 5 * time.Minute

// maxCredentials bounds the gate's memory.
//
// A household has a handful of credentials; a caller trying to grow this map
// has to get the origin to accept each one first, so this is a backstop rather
// than a defence. It is here because an unbounded map that only ever gets
// written by "something good happened" is exactly the kind that nobody notices
// growing.
const maxCredentials = 512

// cacheGate remembers which credentials the origin has recently accepted.
//
// The point is to answer "may this caller have cached bytes?" without a round
// trip, because the request that makes this worth doing is a grid of two
// hundred covers. Asking the origin about each one would cost more than the
// cache saves.
//
// A caller the gate does not recognise is not refused — it is simply not served
// from cache. It falls through to the path that talks to the origin, which
// either rejects it or serves it, and a success admits the credential. So the
// gate is a fast path for the authorized, never a second authority on who is.
type cacheGate struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	now   func() time.Time // injectable for tests
	ttl   time.Duration
	limit int
}

func newCacheGate() *cacheGate {
	return &cacheGate{
		seen:  make(map[string]time.Time),
		now:   time.Now,
		ttl:   credentialTTL,
		limit: maxCredentials,
	}
}

// Allows reports whether this request may be answered from cache.
func (g *cacheGate) Allows(r *http.Request) bool {
	fingerprint := credentialFingerprint(r)
	if fingerprint == "" {
		// No credential at all. Never served from cache — that is the whole
		// bypass.
		return false
	}
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	expires, ok := g.seen[fingerprint]
	if !ok {
		return false
	}
	if now.After(expires) {
		delete(g.seen, fingerprint)
		return false
	}
	return true
}

// Admit records that the origin accepted this request's credential.
//
// Called only where the origin has actually answered successfully, so the gate
// can never learn a credential the origin has not itself approved.
func (g *cacheGate) Admit(r *http.Request) {
	fingerprint := credentialFingerprint(r)
	if fingerprint == "" {
		// An anonymous request that succeeded means the route is public. That
		// says nothing about who may read cached bytes, so it teaches the gate
		// nothing.
		return
	}
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(now)
	if len(g.seen) >= g.limit {
		if _, refreshing := g.seen[fingerprint]; !refreshing {
			g.evictOldestLocked()
		}
	}
	g.seen[fingerprint] = now.Add(g.ttl)
}

// sweepLocked drops expired entries. The caller must hold g.mu.
func (g *cacheGate) sweepLocked(now time.Time) {
	for fingerprint, expires := range g.seen {
		if now.After(expires) {
			delete(g.seen, fingerprint)
		}
	}
}

// evictOldestLocked makes room for one new entry. The caller must hold g.mu.
func (g *cacheGate) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for fingerprint, expires := range g.seen {
		if oldestKey == "" || expires.Before(oldest) {
			oldestKey, oldest = fingerprint, expires
		}
	}
	if oldestKey != "" {
		delete(g.seen, oldestKey)
	}
}

// credentialFingerprint reduces a request's credentials to one opaque string.
//
// It must cover every scheme the origin accepts, because a scheme it misses is
// a caller the gate can never recognise — that is a performance bug, not a
// security one, but it is a silent one. The list mirrors
// classify.CacheKeyQuery, which strips exactly these from the cache key; the
// two are opposite halves of the same fact and want to stay in step.
//
// The result is hashed so the process is not holding a pile of live bearer
// tokens in a map keyed by their plaintext.
func credentialFingerprint(r *http.Request) string {
	parts := make([]string, 0, 4)

	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		parts = append(parts, "h:"+header)
	}
	if header := strings.TrimSpace(r.Header.Get("X-Samo-Token")); header != "" {
		parts = append(parts, "x:"+header)
	}

	query := r.URL.Query()
	// Native stream token, then the Subsonic credential set. Sorted so the
	// same credentials always render the same string regardless of the order
	// the client sent them in.
	for _, key := range []string{"stream_token", "streamtoken", "u", "p", "t", "s", "apikey"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			parts = append(parts, key+":"+value)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)

	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// guardedCache is the handler's view of the cache.
//
// It exists so that reading a cached body REQUIRES a request to authorize it.
// The handler holds one of these rather than a *cache.Cache, so "did I check
// the caller?" is not a question a call site can get wrong: there is no read
// method that does not take a *http.Request, and every one of them refuses
// before it touches the disk.
//
// Writes are ungated on purpose. Getting bytes INTO the cache already required
// a successful origin fetch, so the authorization happened upstream of here.
type guardedCache struct {
	store *cache.Cache
	gate  *cacheGate
}

func newGuardedCache(store *cache.Cache) *guardedCache {
	return &guardedCache{store: store, gate: newCacheGate()}
}

// Peek reports whether a complete entry exists and returns its metadata,
// without opening the body.
//
// Ungated on purpose, and the distinction is the point: the metadata is a
// content type and a pair of validators, not the protected content. The audio
// path needs it before it can revalidate, and revalidation is what authorizes
// that path — gating this would force the body's authorization to happen before
// the request that grants it, which costs a full re-fetch of the source on
// every stream-token rotation.
func (c *guardedCache) Peek(key string) (cache.Meta, bool) {
	file, meta, ok := c.store.Open(key)
	if !ok {
		return cache.Meta{}, false
	}
	// Metadata only. The body is what Open is for.
	file.Close()
	return meta, true
}

// Open returns a complete cache entry, but only to a caller the origin has
// recently accepted. An unrecognised caller gets ok=false and takes the
// origin path, which is what authorizes them.
func (c *guardedCache) Open(r *http.Request, key string) (*os.File, cache.Meta, bool) {
	if !c.gate.Allows(r) {
		return nil, cache.Meta{}, false
	}
	return c.store.Open(key)
}

// Follow attaches to an encode already in flight, under the same rule as Open.
//
// This one is easy to overlook — it looks like an optimisation rather than a
// read — and overlooking it is how an unauthenticated caller could be handed
// live audio for as long as somebody else's transcode was running.
func (c *guardedCache) Follow(r *http.Request, key string) (io.ReadCloser, cache.Meta, bool) {
	if !c.gate.Allows(r) {
		return nil, cache.Meta{}, false
	}
	return c.store.Follow(key)
}

// Begin starts a new entry. No gate: see the type comment.
func (c *guardedCache) Begin(key string, meta cache.Meta) (*cache.Sink, bool) {
	return c.store.Begin(key, meta)
}

// Drop removes an entry. No gate: dropping is a write.
func (c *guardedCache) Drop(key string) { c.store.Drop(key) }

// Admit records that the origin accepted this request's credential.
func (c *guardedCache) Admit(r *http.Request) { c.gate.Admit(r) }

// Allows reports whether the gate already knows this caller, so a call site can
// decide whether it needs to ask the origin. It grants nothing on its own.
func (c *guardedCache) Allows(r *http.Request) bool { return c.gate.Allows(r) }
