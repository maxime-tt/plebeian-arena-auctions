package main

// Durable per-relay ingest cursor (handover §2, workstream B).
//
// The observer used to subscribe `since=beginning` on every start: a restart
// re-read the relay's whole history and raced the node's readiness, and an event
// waiting on the relay when the chain was down was simply dropped.  The cursor
// records the last event processed per relay, so a restart resumes from exactly
// there.
//
// It is deliberately not the observer's `lastSeen`: `lastSeen` is a single
// high-water mark advanced on success, while the cursor is keyed by relay URL
// (a deployment may follow several), is persisted, and advances on a terminal
// rejection too — the relay's copy of a rejected event must not be re-read
// forever.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CursorRecorder is how the observer persists how far it has processed a relay.
// relayCursor satisfies it; a test uses a fake.
type CursorRecorder interface {
	Advance(url string, createdAt int64, eventID string) error
}

// cursorEntry is the last event processed on one relay.  created_at is what a
// re-subscription asks from (minus a second of slack), event_id is what the
// startup log names as the resume point.
type cursorEntry struct {
	CreatedAt int64  `json:"created_at"`
	EventID   string `json:"event_id"`
}

// relayCursor is a JSON object keyed by relay URL, written atomically so a crash
// mid-write can never leave a torn file behind.
type relayCursor struct {
	path string

	mu      sync.Mutex
	entries map[string]cursorEntry
}

// loadRelayCursor reads the cursor at path.  A missing or empty file is not an
// error: it means "nothing processed yet", which is exactly the empty cursor.
func loadRelayCursor(path string) (*relayCursor, error) {
	cursor := &relayCursor{path: path, entries: map[string]cursorEntry{}}
	if path == "" {
		return cursor, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cursor, nil
		}
		return nil, err
	}
	if len(raw) == 0 {
		return cursor, nil
	}
	if err := json.Unmarshal(raw, &cursor.entries); err != nil {
		return nil, err
	}
	if cursor.entries == nil {
		cursor.entries = map[string]cursorEntry{}
	}
	return cursor, nil
}

// since is the resume point for url: the created_at to subscribe from and the
// event id the startup log names.  A zero time means "unknown, read from the
// beginning".
func (cursor *relayCursor) since(url string) (time.Time, string) {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	entry, ok := cursor.entries[url]
	if !ok || entry.CreatedAt <= 0 {
		return time.Time{}, ""
	}
	return time.Unix(entry.CreatedAt, 0).UTC(), entry.EventID
}

// Advance records that the event at createdAt/eventID has been processed on url,
// and persists the cursor.  It is monotonic per URL: an older created_at never
// overwrites a newer one, so a relay replay or an out-of-order delivery cannot
// rewind the resume point.
func (cursor *relayCursor) Advance(url string, createdAt int64, eventID string) error {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if current, ok := cursor.entries[url]; ok {
		if createdAt < current.CreatedAt {
			return nil
		}
		if createdAt == current.CreatedAt && current.EventID == eventID {
			return nil
		}
	}
	cursor.entries[url] = cursorEntry{CreatedAt: createdAt, EventID: eventID}
	return cursor.persistLocked()
}

// persistLocked writes the cursor atomically: temp file in the same directory,
// write, fsync, close, rename, then fsync the directory — the same shape
// snapshot.go uses, and for the same reason (a rename is atomic, a partial
// overwrite is not).  Called once per processed event, so it must stay cheap.
func (cursor *relayCursor) persistLocked() error {
	if cursor.path == "" {
		return nil
	}
	dir := filepath.Dir(cursor.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(cursor.entries)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dir, ".cursor-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), cursor.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
