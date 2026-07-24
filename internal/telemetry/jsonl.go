package telemetry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// timeLayout matches the HTTP API's wire timestamp format so exported telemetry and
// live responses agree on how instants are rendered.
const timeLayout = "2006-01-02T15:04:05.000Z"

// rotatedLayout stamps rotated files. Fixed-width and second-plus-nanosecond
// precise, so lexical order of rotated names is chronological order and two
// rotations can never collide on a name.
const rotatedLayout = "20060102T150405.000000000"

// fetchLine and aggregateLine are the JSONL projections of the record types: both
// kinds share one stream, discriminated by "type". Cells are hex-encoded H3 indexes
// (the same rendering as /_ops_/cells and /_ops_/pairs), never coordinates.
type fetchLine struct {
	Type    string `json:"type"` // "fetch"
	TS      string `json:"ts"`
	Origin  string `json:"origin"`
	Dest    string `json:"dest"`
	Profile string `json:"profile"`
	Source  string `json:"source"`
	Area    int64  `json:"area"`
	Res     int    `json:"res"`
	Stale   bool   `json:"stale"`
}

type aggregateLine struct {
	Type        string `json:"type"` // "demand"
	BucketStart string `json:"bucketStart"`
	Origin      string `json:"origin"`
	Dest        string `json:"dest"`
	Profile     string `json:"profile"`
	BucketSec   int64  `json:"bucketSec"`
	Area        int64  `json:"area"`
	Res         int    `json:"res"`
	Count       int64  `json:"count"`
	Cache       int64  `json:"cache"`
	SameCell    int64  `json:"sameCell"`
	Demand      int64  `json:"demand"`
	Stale       int64  `json:"stale"`
}

// JSONLSink appends telemetry records to a newline-delimited JSON file, rotating it
// by size. Rotated files are renamed path.<timestamp> and the oldest are pruned so
// at most maxFiles rotated siblings are retained alongside the live file. Writes go
// through a bufio.Writer; Flush (called on the Recorder's tick) makes the file
// tail-able, and Close flushes and closes it.
type JSONLSink struct {
	f        *os.File
	w        *bufio.Writer
	path     string
	written  int64
	maxBytes int64
	maxFiles int
	mu       sync.Mutex
}

// NewJSONLSink opens (creating if needed, appending if present) the JSONL file at
// path. maxBytes is the rotation threshold and maxFiles the number of rotated files
// retained.
func NewJSONLSink(path string, maxBytes int64, maxFiles int) (*JSONLSink, error) {
	s := &JSONLSink{
		path:     filepath.Clean(path),
		maxBytes: maxBytes,
		maxFiles: maxFiles,
	}

	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("telemetry: creating sink directory: %w", err)
		}
	}
	if err := s.openLocked(); err != nil {
		return nil, err
	}

	return s, nil
}

// WriteFetch appends one raw fetch event line.
func (s *JSONLSink) WriteFetch(ev *FetchEvent) error {
	return s.writeLine(fetchLine{
		Type:    "fetch",
		TS:      ev.At.UTC().Format(timeLayout),
		Area:    int64(ev.Key.Area),
		Origin:  ev.Key.Origin.String(),
		Dest:    ev.Key.Dest.String(),
		Res:     ev.Key.Res,
		Profile: string(ev.Key.Profile),
		Source:  ev.Source,
		Stale:   ev.Stale,
	})
}

// WriteAggregate appends one aggregated demand-count line.
func (s *JSONLSink) WriteAggregate(rec *AggregateRecord) error {
	return s.writeLine(aggregateLine{
		Type:        "demand",
		BucketStart: rec.BucketStart.UTC().Format(timeLayout),
		BucketSec:   int64(rec.BucketSize / time.Second),
		Area:        int64(rec.Key.Area),
		Origin:      rec.Key.Origin.String(),
		Dest:        rec.Key.Dest.String(),
		Res:         rec.Key.Res,
		Profile:     string(rec.Key.Profile),
		Count:       rec.Count,
		Cache:       rec.Cache,
		SameCell:    rec.SameCell,
		Demand:      rec.Demand,
		Stale:       rec.Stale,
	})
}

// Flush pushes buffered lines to the OS so the file can be tailed between rotations.
func (s *JSONLSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.w == nil {
		return nil
	}

	return s.w.Flush()
}

// Close flushes and closes the live file. Safe to call more than once.
func (s *JSONLSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.f == nil {
		return nil
	}

	flushErr := s.w.Flush()
	closeErr := s.f.Close()
	s.f, s.w = nil, nil
	if flushErr != nil {
		return flushErr
	}

	return closeErr
}

// writeLine marshals one record, rotating first if the line would push the live
// file past maxBytes. A line larger than maxBytes on its own is still written (to a
// fresh file) rather than lost.
func (s *JSONLSink) writeLine(v any) error {
	line, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("telemetry: marshaling record: %w", err)
	}
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.f == nil {
		return fmt.Errorf("telemetry: sink is closed")
	}
	if s.written > 0 && s.written+int64(len(line)) > s.maxBytes {
		if err = s.rotateLocked(); err != nil {
			return err
		}
	}

	n, err := s.w.Write(line)
	s.written += int64(n)
	if err != nil {
		return fmt.Errorf("telemetry: writing record: %w", err)
	}

	return nil
}

// openLocked opens the live file for appending and resumes the byte count from its
// current size, so rotation thresholds survive a restart.
func (s *JSONLSink) openLocked() error {
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("telemetry: opening sink file: %w", err)
	}

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("telemetry: stating sink file: %w", err)
	}

	s.f = f
	s.w = bufio.NewWriter(f)
	s.written = info.Size()

	return nil
}

// rotateLocked closes the live file, renames it aside with a timestamp, reopens a
// fresh one, and prunes the oldest rotated siblings beyond maxFiles.
func (s *JSONLSink) rotateLocked() error {
	if err := s.w.Flush(); err != nil {
		return fmt.Errorf("telemetry: flushing before rotation: %w", err)
	}
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("telemetry: closing before rotation: %w", err)
	}
	s.f, s.w = nil, nil

	rotated := s.path + "." + time.Now().UTC().Format(rotatedLayout)
	if err := os.Rename(s.path, rotated); err != nil {
		return fmt.Errorf("telemetry: rotating sink file: %w", err)
	}

	if err := s.openLocked(); err != nil {
		return err
	}
	s.pruneLocked()

	return nil
}

// pruneLocked deletes the oldest rotated files until at most maxFiles remain. The
// rotation stamp is fixed-width, so lexical order is age order. Prune failures are
// swallowed: losing old telemetry beats failing a write.
func (s *JSONLSink) pruneLocked() {
	rotated, err := filepath.Glob(s.path + ".*")
	if err != nil || len(rotated) <= s.maxFiles {
		return
	}

	sort.Strings(rotated)
	for _, old := range rotated[:len(rotated)-s.maxFiles] {
		if removeErr := os.Remove(old); removeErr != nil {
			continue // best effort; the next rotation retries
		}
	}
}
