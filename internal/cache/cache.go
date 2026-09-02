// Package cache stores transcoded audio on disk and lets readers follow an
// entry that is still being written.
//
// The follow-while-writing part is what makes transcoding usable rather than
// merely possible. A cold FLAC -> Opus transcode of a long audiobook chapter
// takes real seconds; if the first listener had to wait for it to finish, every
// uncached track would open with a stall. Instead the transcode writes into a
// temp file and any number of readers tail it, so playback starts as soon as
// ffmpeg has produced a few kilobytes.
//
// The producer deliberately does NOT run under the requesting client's context.
// A listener who skips a track three seconds in should still leave a complete
// cache entry behind — otherwise skipping around an album leaves nothing cached
// and every later play pays the transcode again.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Meta is what we remember about a cached response besides its bytes.
type Meta struct {
	ContentType string `json:"contentType"`
	// Origin validators, kept so a cache hit can be revalidated cheaply against
	// the LAN origin. samo-server serves audio with `Cache-Control: private,
	// max-age=3600` precisely because a file's contents can change under a
	// stable URL — a re-tag or a replaced download — so a cache that never
	// revalidates would serve stale audio indefinitely.
	OriginETag         string    `json:"originETag,omitempty"`
	OriginLastModified string    `json:"originLastModified,omitempty"`
	OriginLength       int64     `json:"originLength,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
}

// Cache is a size-bounded directory of cached responses.
type Cache struct {
	dir      string
	maxBytes int64

	mu   sync.Mutex
	live map[string]*Sink
}

// New prepares the cache directory.
func New(dir string, maxBytes int64) (*Cache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create cache dir %s: %w", dir, err)
	}
	// A stale temp file is a transcode that was interrupted by a restart. It
	// can never be completed, and nothing will ever read it.
	sweepTemps(dir)
	return &Cache{dir: dir, maxBytes: maxBytes, live: map[string]*Sink{}}, nil
}

// Key derives a stable filename-safe key from the parts that identify a
// response. Callers pass whatever combination genuinely selects the bytes —
// path, filtered query, transcode profile — and never anything that rotates.
func Key(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		// Length-prefix each part so ("ab","c") and ("a","bc") cannot collide.
		fmt.Fprintf(hash, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (c *Cache) dataPath(key string) string { return filepath.Join(c.dir, key+".bin") }
func (c *Cache) metaPath(key string) string { return filepath.Join(c.dir, key+".json") }

// Open returns a complete cache entry. The caller owns the returned file and
// must close it.
func (c *Cache) Open(key string) (*os.File, Meta, bool) {
	var meta Meta
	raw, err := os.ReadFile(c.metaPath(key))
	if err != nil {
		return nil, meta, false
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, meta, false
	}
	file, err := os.Open(c.dataPath(key))
	if err != nil {
		return nil, meta, false
	}
	// Touch so eviction sees this as recently used. A failure here only costs
	// eviction accuracy, so it is not worth failing the read over.
	now := time.Now()
	_ = os.Chtimes(c.dataPath(key), now, now)
	return file, meta, true
}

// Follow returns a reader over an entry that is currently being written, if
// one is in flight for this key.
func (c *Cache) Follow(key string) (io.ReadCloser, Meta, bool) {
	c.mu.Lock()
	sink, ok := c.live[key]
	c.mu.Unlock()
	if !ok {
		return nil, Meta{}, false
	}
	reader, err := sink.Reader()
	if err != nil {
		return nil, Meta{}, false
	}
	return reader, sink.meta, true
}

// Begin registers a new in-flight entry. It reports false when another producer
// already holds this key, in which case the caller should Follow instead.
func (c *Cache) Begin(key string, meta Meta) (*Sink, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.live[key]; exists {
		return nil, false
	}
	file, err := os.CreateTemp(c.dir, "wip-*.tmp")
	if err != nil {
		return nil, false
	}
	meta.CreatedAt = time.Now()
	sink := &Sink{cache: c, key: key, file: file, meta: meta}
	sink.cond = sync.NewCond(&sink.mu)
	c.live[key] = sink
	return sink, true
}

// finish moves a completed sink into the cache proper, or discards it.
func (c *Cache) finish(sink *Sink, failed bool) {
	c.mu.Lock()
	delete(c.live, sink.key)
	c.mu.Unlock()

	tempName := sink.file.Name()
	if failed {
		_ = os.Remove(tempName)
		return
	}
	// Metadata is written first and the data file renamed second, so a crash
	// between the two leaves an orphan .json rather than a .bin that Open would
	// happily serve with no metadata at all.
	raw, err := json.Marshal(sink.meta)
	if err != nil {
		_ = os.Remove(tempName)
		return
	}
	if err := os.WriteFile(c.metaPath(sink.key), raw, 0o644); err != nil {
		_ = os.Remove(tempName)
		return
	}
	if err := os.Rename(tempName, c.dataPath(sink.key)); err != nil {
		_ = os.Remove(tempName)
		_ = os.Remove(c.metaPath(sink.key))
		return
	}
	c.evict()
}

// evict trims the cache to maxBytes, oldest-accessed first.
func (c *Cache) evict() {
	if c.maxBytes <= 0 {
		return
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	type item struct {
		key  string
		size int64
		used time.Time
	}
	var items []item
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".bin" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{
			key:  name[:len(name)-len(".bin")],
			size: info.Size(),
			used: info.ModTime(),
		})
		total += info.Size()
	}
	if total <= c.maxBytes {
		return
	}
	sort.Slice(items, func(a, b int) bool { return items[a].used.Before(items[b].used) })
	for _, entry := range items {
		if total <= c.maxBytes {
			return
		}
		// Never evict something a reader is currently following.
		c.mu.Lock()
		_, inFlight := c.live[entry.key]
		c.mu.Unlock()
		if inFlight {
			continue
		}
		_ = os.Remove(c.dataPath(entry.key))
		_ = os.Remove(c.metaPath(entry.key))
		total -= entry.size
	}
}

// Drop removes an entry, used when revalidation shows it is stale.
func (c *Cache) Drop(key string) {
	_ = os.Remove(c.dataPath(key))
	_ = os.Remove(c.metaPath(key))
}

func sweepTemps(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".tmp" {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// Sink is the write end of an in-flight cache entry.
type Sink struct {
	cache *Cache
	key   string
	file  *os.File
	meta  Meta

	mu     sync.Mutex
	cond   *sync.Cond
	size   int64
	done   bool
	failed bool
}

// Write appends to the entry and wakes every follower.
func (s *Sink) Write(p []byte) (int, error) {
	n, err := s.file.Write(p)
	if n > 0 {
		s.mu.Lock()
		s.size += int64(n)
		s.mu.Unlock()
		s.cond.Broadcast()
	}
	return n, err
}

// Close finalises the entry. A non-nil err discards it: a truncated transcode
// must never be cached, or the same partial track plays forever.
func (s *Sink) Close(err error) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	s.failed = err != nil
	s.mu.Unlock()
	s.cond.Broadcast()

	_ = s.file.Sync()
	_ = s.file.Close()
	s.cache.finish(s, err != nil)
}

// Reader returns an independent tailing reader over the entry.
func (s *Sink) Reader() (io.ReadCloser, error) {
	file, err := os.Open(s.file.Name())
	if err != nil {
		return nil, err
	}
	return &follower{sink: s, file: file}, nil
}

// follower reads an entry as it is produced, blocking at the write frontier
// until more bytes arrive or the producer finishes.
type follower struct {
	sink   *Sink
	file   *os.File
	offset int64
}

func (f *follower) Read(p []byte) (int, error) {
	for {
		f.sink.mu.Lock()
		for f.offset >= f.sink.size && !f.sink.done {
			f.sink.cond.Wait()
		}
		available := f.sink.size - f.offset
		done, failed := f.sink.done, f.sink.failed
		f.sink.mu.Unlock()

		if available <= 0 {
			if failed {
				return 0, errors.New("transcode failed")
			}
			if done {
				return 0, io.EOF
			}
			continue
		}
		if int64(len(p)) > available {
			p = p[:available]
		}
		n, err := f.file.ReadAt(p, f.offset)
		f.offset += int64(n)
		// ReadAt reports EOF when it fills less than the buffer, which here just
		// means we caught up with the producer — not that the entry is over.
		if errors.Is(err, io.EOF) && n > 0 {
			err = nil
		}
		return n, err
	}
}

func (f *follower) Close() error { return f.file.Close() }
