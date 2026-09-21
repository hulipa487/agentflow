// Package logfile is the append-log backend for the engine's log plane: the
// message journal and the per-call usage detail, written as newline-delimited
// JSON, one file per UTC day, pruned by deleting whole files.
//
// It is for a deployment that writes a lot into those two planes and would
// rather not put it in the transactional store: an append is a write to the end
// of one file per day, a read is a scan of the days inside the window, and
// retention is a file deletion rather than a DELETE of a million rows.
//
// What it is not, and the limits that follow from being plain files:
//
//   - A machine crash can lose the tail. Records are written to the file
//     directly — no buffering, so a process crash loses nothing — but they are
//     not fsynced, and an audit trail that is missing its last second is worth
//     knowing about rather than hiding. Both journal call sites already treat a
//     failed write as best-effort.
//   - One process owns the directory. Concurrent writers would interleave
//     records without a lock; the engine opens this per instance.
//   - A journal here is per instance, which fragments a fleet's audit trail —
//     the opposite of what an audit trail is for. A fleet keeps the journal in
//     the shared store; this backend is for a single-instance deployment that
//     wants the write volume out of its main database.
package logfile

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"agentflow/internal/core/runtime"
)

// The two families of file this backend keeps. The day is the UTC day of the
// record's own timestamp, never the wall clock at write time: a record that
// arrives late belongs to the day it happened, which is what makes a prune by
// date mean what it says.
const (
	journalPrefix = "journal-"
	eventsPrefix  = "events-"
	fileSuffix    = ".jsonl"
)

// Log is the append-log plane: a Journal and an EventLog over one directory.
type Log struct {
	dir string

	mu     sync.Mutex
	open   map[string]*os.File
	closed bool
}

// Open prepares the directory. Nothing is written until the first record.
func Open(dir string) (*Log, error) {
	if dir == "" {
		return nil, fmt.Errorf("logfile: empty directory")
	}
	if err := os.MkdirAll(dir, 0750); err != nil {
		return nil, fmt.Errorf("logfile: create %s: %w", dir, err)
	}
	return &Log{dir: dir, open: map[string]*os.File{}}, nil
}

// Dir reports where the log lives, for a log line.
func (l *Log) Dir() string { return l.dir }

// Close closes every open day file. A write after Close is an error rather than
// a silent no-op.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	var first error
	for name, f := range l.open {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
		delete(l.open, name)
	}
	return first
}

// --- journal ----------------------------------------------------------------

// RecordMessage appends one entry. The entry's own timestamp — or now, when it
// carries none — decides which day file it lands in.
func (l *Log) RecordMessage(ctx context.Context, e runtime.JournalEntry) error {
	if e.Ts == 0 {
		e.Ts = time.Now().Unix()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("logfile: encode journal entry: %w", err)
	}
	return l.append(journalPrefix, runtime.DayKey(time.Unix(e.Ts, 0)), line)
}

// ListMessages reads journal entries matching the filter, newest first. Days
// outside the filter's window are not opened at all, and the scan stops once it
// has enough rows: a filter with no window reads from the newest day backwards
// until the limit is met.
func (l *Log) ListMessages(ctx context.Context, f runtime.JournalFilter) ([]runtime.JournalEntry, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	days, err := l.days(journalPrefix, f)
	if err != nil {
		return nil, err
	}

	out := []runtime.JournalEntry{}
	for _, day := range days { // newest first
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := l.readJournal(day)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if !journalMatches(e, f) {
				continue
			}
			out = append(out, e)
		}
		if len(out) >= limit {
			break // older days hold older rows; nothing below can displace these
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Ts != out[j].Ts {
			return out[i].Ts > out[j].Ts
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneMessages deletes whole day files older than the cutoff, and reports how
// many entries went with them. It never touches the current day: a retention
// that rounds to "today" would otherwise delete the file being written.
func (l *Log) PruneMessages(ctx context.Context, cutoff time.Time) (int64, error) {
	return l.prune(journalPrefix, cutoff, true)
}

// JournalRowCount counts every entry on disk. It is a scan of every day file,
// which is why nothing on a request path calls it: the size of the audit trail
// is an operator question, not a hot one.
func (l *Log) JournalRowCount(ctx context.Context) (int64, error) {
	days, err := l.days(journalPrefix, runtime.JournalFilter{})
	if err != nil {
		return 0, err
	}
	var n int64
	for _, day := range days {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		entries, err := l.readJournal(day)
		if err != nil {
			return 0, err
		}
		n += int64(len(entries))
	}
	return n, nil
}

// --- per-call usage detail --------------------------------------------------

// eventLine is one usage event as stored. The store's UsageEvent carries no
// user id — a table's WHERE clause supplies it — but a line in a file has to
// carry what the read side filters on.
type eventLine struct {
	UserID string             `json:"user_id"`
	Event  runtime.UsageEvent `json:"event"`
}

// AppendEvent appends one per-call detail record.
func (l *Log) AppendEvent(userID string, ev runtime.UsageEvent) error {
	ts := ev.Ts
	if ts == 0 {
		ts = time.Now().Unix()
		ev.Ts = ts
	}
	line, err := json.Marshal(eventLine{UserID: userID, Event: ev})
	if err != nil {
		return fmt.Errorf("logfile: encode usage event: %w", err)
	}
	return l.append(eventsPrefix, runtime.DayKey(time.Unix(ts, 0)), line)
}

// UsageEvents returns one user's recent calls, newest first.
func (l *Log) UsageEvents(userID string, since time.Time, limit int) ([]runtime.UsageEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	f := runtime.JournalFilter{Since: since.Unix()}
	days, err := l.days(eventsPrefix, f)
	if err != nil {
		return nil, err
	}

	out := []runtime.UsageEvent{}
	for _, day := range days {
		lines, err := l.readEvents(day)
		if err != nil {
			return nil, err
		}
		for _, ln := range lines {
			if ln.UserID != userID || ln.Event.Ts < f.Since {
				continue
			}
			out = append(out, ln.Event)
		}
		if len(out) >= limit {
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PruneEvents deletes whole day files older than the cutoff, reporting how many
// records went with them.
func (l *Log) PruneEvents(before time.Time) (int64, error) {
	return l.prune(eventsPrefix, before, true)
}

// --- files ------------------------------------------------------------------

// append writes one line to the day file, opening it if needed.
func (l *Log) append(prefix, day string, line []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return fmt.Errorf("logfile: the log is closed")
	}
	name := prefix + day + fileSuffix
	f, ok := l.open[name]
	if !ok {
		var err error
		// O_APPEND: two records never overwrite each other, and the file is
		// created on first use so a day that never happens leaves nothing.
		f, err = os.OpenFile(filepath.Join(l.dir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
		if err != nil {
			return fmt.Errorf("logfile: open %s: %w", name, err)
		}
		l.open[name] = f
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("logfile: append to %s: %w", name, err)
	}
	return nil
}

// days lists the day files of one family, newest first, restricted to the
// window the filter names: days before its Since, or at or after its Until, are
// not opened at all.
func (l *Log) days(prefix string, f runtime.JournalFilter) ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("logfile: read %s: %w", l.dir, err)
	}
	var since, until string
	if f.Since > 0 {
		since = runtime.DayKey(time.Unix(f.Since, 0))
	}
	if f.Until > 0 {
		until = runtime.DayKey(time.Unix(f.Until, 0))
	}
	var days []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, prefix), fileSuffix)
		// Both bounds are inclusive at day granularity: a day equal to the
		// window's first or last day holds rows inside the window, so only days
		// wholly outside it are skipped. (An exclusive test on Until would drop
		// the file for a window that begins and ends inside one day.)
		if since != "" && day < since {
			continue
		}
		if until != "" && day > until {
			continue
		}
		days = append(days, day)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(days))) // the day format sorts as text
	return days, nil
}

// readJournal reads one day file. A line that will not decode is skipped and
// counted, not fatal: a truncated tail from a crash mid-write must not make the
// rest of the day unreadable.
func (l *Log) readJournal(day string) ([]runtime.JournalEntry, error) {
	var out []runtime.JournalEntry
	err := l.scan(journalPrefix+day+fileSuffix, func(line []byte) {
		var e runtime.JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return
		}
		out = append(out, e)
	})
	return out, err
}

func (l *Log) readEvents(day string) ([]eventLine, error) {
	var out []eventLine
	err := l.scan(eventsPrefix+day+fileSuffix, func(line []byte) {
		var ln eventLine
		if err := json.Unmarshal(line, &ln); err != nil {
			return
		}
		out = append(out, ln)
	})
	return out, err
}

// scan calls fn for every complete line of a day file.
func (l *Log) scan(name string, fn func([]byte)) error {
	f, err := os.Open(filepath.Join(l.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // pruned between listing and reading
		}
		return fmt.Errorf("logfile: open %s: %w", name, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// A journal entry carries a message's text and its attachment descriptors,
	// so the line buffer has to be generous — the scanner's default 64 KiB is
	// one long message away from truncating a day.
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		fn(sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("logfile: read %s: %w", name, err)
	}
	return nil
}

// prune deletes the day files of one family that are older than the cutoff,
// optionally counting the lines it removed. The current day is never deleted:
// it holds the file being written.
func (l *Log) prune(prefix string, cutoff time.Time, count bool) (int64, error) {
	today := runtime.DayKey(time.Now())
	boundary := runtime.DayKey(cutoff)

	l.mu.Lock()
	defer l.mu.Unlock()
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("logfile: read %s: %w", l.dir, err)
	}

	var removed int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, fileSuffix) {
			continue
		}
		day := strings.TrimSuffix(strings.TrimPrefix(name, prefix), fileSuffix)
		if day >= boundary || day == today {
			continue
		}
		// Close before removing: an open handle would keep writing to a deleted
		// file on Unix, and on Windows the remove would simply fail.
		if f, ok := l.open[name]; ok {
			_ = f.Close()
			delete(l.open, name)
		}
		if count {
			n, err := l.countLines(name)
			if err != nil {
				return removed, err
			}
			removed += n
		}
		if err := os.Remove(filepath.Join(l.dir, name)); err != nil && !os.IsNotExist(err) {
			return removed, fmt.Errorf("logfile: remove %s: %w", name, err)
		}
	}
	return removed, nil
}

// countLines counts a file's lines before it is deleted, so a prune can report
// what it dropped the way the SQL backends' row count does. A file that has
// already vanished counts as nothing.
func (l *Log) countLines(name string) (int64, error) {
	f, err := os.Open(filepath.Join(l.dir, name))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("logfile: open %s: %w", name, err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var n int64
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			n++
		}
	}
	return n, sc.Err()
}

// journalMatches applies a filter to one entry, mirroring the SQL WHERE clause
// the store backends use — including that a zero field is unset.
func journalMatches(e runtime.JournalEntry, f runtime.JournalFilter) bool {
	if f.UserUUID != "" && e.UserUUID != f.UserUUID {
		return false
	}
	if f.SessionID != "" && e.SessionID != f.SessionID {
		return false
	}
	if f.Direction != "" && e.Direction != f.Direction {
		return false
	}
	if f.Since > 0 && e.Ts < f.Since {
		return false
	}
	if f.Until > 0 && e.Ts >= f.Until {
		return false
	}
	return true
}
