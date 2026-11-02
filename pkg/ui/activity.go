package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

const (
	// activityFile is the interface's record of who did what, one JSON line each, in the
	// appliance's folder.
	activityFile = "activity.jsonl"
	// activityKept is how long a line is kept.
	activityKept = 90 * day
	// activityMaxBytes bounds the file. Past it, the oldest half goes.
	activityMaxBytes = 8 << 20
	// activityInMemory bounds the lines kept without an appliance, as in development.
	activityInMemory             = 2000
	activityFileMode fs.FileMode = 0o600
	// day is activityKept's unit, as the pages and the CLI count it.
	day = 24 * time.Hour
	// scanStart is a line reader's first buffer.
	scanStart = 64 << 10
)

// activityEntry is one thing a user did, or tried, through the interface.
type activityEntry struct {
	At      time.Time `json:"at"`
	User    string    `json:"user"`
	What    string    `json:"what"`
	Cluster string    `json:"cluster,omitempty"`
	// Failed is set when the action ended in an error.
	Failed bool `json:"failed,omitempty"`
	// Details are the line's other attributes, as text.
	Details map[string]string `json:"details,omitempty"`
}

// activityLog keeps the entries, in a file when there is a folder for one.
type activityLog struct {
	mu   sync.Mutex
	path string
	size int64
	// maxBytes is activityMaxBytes, smaller in tests.
	maxBytes int64
	// mem holds the entries without a file, oldest first.
	mem []activityEntry
}

func newActivityLog(dir string) *activityLog {
	l := &activityLog{maxBytes: activityMaxBytes}
	if dir == "" {
		return l
	}
	l.path = filepath.Join(dir, activityFile)
	if fi, err := os.Stat(l.path); err == nil {
		l.size = fi.Size()
	}
	return l
}

// add appends an entry.
func (l *activityLog) add(e *activityEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" {
		l.mem = append(l.mem, *e)
		if over := len(l.mem) - activityInMemory; over > 0 {
			l.mem = slices.Delete(l.mem, 0, over)
		}
		return nil
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err //nolint:wrapcheck // an entry of strings and a time always encodes
	}
	line = append(line, '\n')
	if l.size+int64(len(line)) > l.maxBytes {
		if terr := l.trim(e.At); terr != nil {
			return terr
		}
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, activityFileMode)
	if err != nil {
		return err //nolint:wrapcheck // the logger reports it as the file's
	}
	n, werr := f.Write(line)
	l.size += int64(n)
	return errors.Join(werr, f.Close())
}

// trim keeps the newer half of the file's entries, and none older than activityKept.
func (l *activityLog) trim(now time.Time) error {
	all, err := l.readFile()
	if err != nil {
		return err
	}
	all = all[len(all)/2:]
	all = slices.DeleteFunc(all, func(e activityEntry) bool { return now.Sub(e.At) > activityKept })
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for i := range all {
		if err := enc.Encode(&all[i]); err != nil {
			return err //nolint:wrapcheck // as in add
		}
	}
	tmp := l.path + ".new"
	if err := os.WriteFile(tmp, b.Bytes(), activityFileMode); err != nil {
		return err //nolint:wrapcheck // as in add
	}
	if err := os.Rename(tmp, l.path); err != nil {
		return err //nolint:wrapcheck // as in add
	}
	l.size = int64(b.Len())
	return nil
}

// readFile reads the file's entries, oldest first. A line that does not read is skipped.
func (l *activityLog) readFile() ([]activityEntry, error) {
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err //nolint:wrapcheck // the caller says which file
	}
	defer func() { _ = f.Close() }()
	var out []activityEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, scanStart), auditLineMax)
	for sc.Scan() {
		var e activityEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err() //nolint:wrapcheck // as above
}

// since returns the entries at or after from that keep says to, newest first, at most limit. The
// API server's changes come in up to a minute late, so the file is not in order of time.
func (l *activityLog) since(from time.Time, limit int, keep func(*activityEntry) bool) ([]activityEntry, error) {
	l.mu.Lock()
	var all []activityEntry
	var err error
	if l.path == "" {
		all = slices.Clone(l.mem)
	} else {
		all, err = l.readFile()
	}
	l.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := []activityEntry{}
	// Newest written first, so entries of the same time stay in that order.
	for i := len(all) - 1; i >= 0; i-- {
		if e := &all[i]; !e.At.Before(from) && keep(e) {
			out = append(out, *e)
		}
	}
	slices.SortStableFunc(out, func(a, b activityEntry) int { return b.At.Compare(a.At) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// activityHandler keeps every line logged with a user in the activity log, beside logging it.
type activityHandler struct {
	slog.Handler

	log   *activityLog
	attrs []slog.Attr
}

// withActivity is the logger. Each line logged with a user also goes into log.
func withActivity(l *slog.Logger, log *activityLog) *slog.Logger {
	return slog.New(activityHandler{Handler: l.Handler(), log: log})
}

// Enabled implements slog.Handler. The activity log takes what users did whatever level the logger
// behind it keeps.
func (h activityHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo || h.Handler.Enabled(ctx, level)
}

// Handle implements slog.Handler.
func (h activityHandler) Handle(ctx context.Context, rec slog.Record) error { //nolint:gocritic // slog.Handler takes the record by value
	if e := activityEntryOf(&rec, h.attrs); e != nil {
		if err := h.log.add(e); err != nil && h.Handler.Enabled(ctx, slog.LevelError) {
			_ = h.Handler.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelError, "keeping the activity log: "+err.Error(), 0))
		}
	}
	if !h.Handler.Enabled(ctx, rec.Level) {
		return nil
	}
	return h.Handler.Handle(ctx, rec) //nolint:wrapcheck // a handler's error is the logger's, passed on as it is
}

// activityEntryOf is a line, with the logger's attributes, as an activity entry, or nil for a line
// no user's action logged.
func activityEntryOf(rec *slog.Record, attrs []slog.Attr) *activityEntry {
	if rec.Level < slog.LevelInfo || rec.Level >= slog.LevelError {
		return nil
	}
	e := &activityEntry{At: rec.Time.UTC(), What: rec.Message}
	take := func(a slog.Attr) bool {
		switch a.Key {
		case "user":
			e.User = a.Value.String()
		case "cluster":
			e.Cluster = a.Value.String()
		case "error":
			// The error's text is the system's, and may say more than a cluster's viewers should see.
			e.Failed = true
		default:
			if e.Details == nil {
				e.Details = map[string]string{}
			}
			e.Details[a.Key] = a.Value.String()
		}
		return true
	}
	for _, a := range attrs {
		take(a)
	}
	rec.Attrs(take)
	if e.User == "" {
		return nil
	}
	return e
}

// WithAttrs implements slog.Handler.
func (h activityHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return activityHandler{Handler: h.Handler.WithAttrs(attrs), log: h.log, attrs: slices.Concat(h.attrs, attrs)}
}

// WithGroup implements slog.Handler. A group's attributes are no user's or cluster's, so the
// activity log takes no line logged within one.
func (h activityHandler) WithGroup(name string) slog.Handler {
	return h.Handler.WithGroup(name)
}
