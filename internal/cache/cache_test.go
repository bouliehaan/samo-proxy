package cache

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestCache(t *testing.T, maxBytes int64) *Cache {
	t.Helper()
	store, err := New(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

// The behaviour the whole transcoding path depends on: a reader can consume an
// entry while it is still being produced, so playback starts before ffmpeg has
// finished.
func TestFollowerReadsWhileEntryIsStillBeingWritten(t *testing.T) {
	store := newTestCache(t, 1<<30)

	sink, ok := store.Begin("key", Meta{ContentType: "audio/ogg"})
	if !ok {
		t.Fatal("Begin reported the key was already in flight")
	}

	reader, err := sink.Reader()
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer reader.Close()

	// The reader is started before any bytes exist, so it must block rather
	// than see a premature EOF.
	got := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(reader)
		got <- string(data)
	}()

	sink.Write([]byte("first "))
	time.Sleep(20 * time.Millisecond)
	sink.Write([]byte("second"))
	sink.Close(nil)

	select {
	case data := <-got:
		if data != "first second" {
			t.Fatalf("follower read %q, want %q", data, "first second")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follower never finished")
	}
}

func TestCompletedEntryIsReadableAfterClose(t *testing.T) {
	store := newTestCache(t, 1<<30)

	sink, _ := store.Begin("key", Meta{ContentType: "audio/ogg"})
	sink.Write([]byte("payload"))
	sink.Close(nil)

	file, meta, ok := store.Open("key")
	if !ok {
		t.Fatal("completed entry was not readable")
	}
	defer file.Close()

	if meta.ContentType != "audio/ogg" {
		t.Errorf("ContentType = %q, want audio/ogg", meta.ContentType)
	}
	data, _ := io.ReadAll(file)
	if string(data) != "payload" {
		t.Errorf("cached bytes = %q, want payload", data)
	}
}

// A truncated encode must never be cached, or the same partial track plays
// forever.
func TestFailedEntryIsDiscarded(t *testing.T) {
	store := newTestCache(t, 1<<30)

	sink, _ := store.Begin("key", Meta{})
	sink.Write([]byte("half a track"))
	sink.Close(io.ErrUnexpectedEOF)

	if _, _, ok := store.Open("key"); ok {
		t.Fatal("a failed encode was committed to the cache")
	}
}

// Two producers must not race on one key: the second is told to follow instead.
func TestBeginRefusesADuplicateKey(t *testing.T) {
	store := newTestCache(t, 1<<30)

	first, ok := store.Begin("key", Meta{})
	if !ok {
		t.Fatal("first Begin failed")
	}
	defer first.Close(nil)

	if _, ok := store.Begin("key", Meta{}); ok {
		t.Fatal("second Begin was allowed to start a duplicate encode")
	}
	if _, _, ok := store.Follow("key"); !ok {
		t.Fatal("in-flight entry could not be followed")
	}
}

func TestEvictionTrimsToTheByteBudget(t *testing.T) {
	store := newTestCache(t, 300)

	// Three 200-byte entries against a 300-byte budget: writing the third must
	// leave the cache under budget.
	for _, key := range []string{"a", "b", "c"} {
		sink, ok := store.Begin(key, Meta{})
		if !ok {
			t.Fatalf("Begin(%s) failed", key)
		}
		sink.Write(make([]byte, 200))
		sink.Close(nil)
		// Distinct mtimes so the LRU ordering is well-defined.
		time.Sleep(10 * time.Millisecond)
	}

	var total int64
	entries, _ := os.ReadDir(store.dir)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".bin" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	if total > 300 {
		t.Fatalf("cache holds %d bytes, over the 300-byte budget", total)
	}
	// The newest entry is the one that must have survived.
	if _, _, ok := store.Open("c"); !ok {
		t.Error("most recent entry was evicted")
	}
}

// A restart mid-encode leaves a temp file that can never be completed.
func TestStaleTempFilesAreSweptOnOpen(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "wip-orphan.tmp")
	if err := os.WriteFile(orphan, []byte("interrupted"), 0o644); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}
	if _, err := New(dir, 1<<30); err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("stale temp file survived startup")
	}
}

// Length-prefixing in Key means adjacent parts cannot be shuffled into a
// collision.
func TestKeyPartsCannotCollide(t *testing.T) {
	if Key("ab", "c") == Key("a", "bc") {
		t.Fatal("Key collided on a boundary shift")
	}
}
