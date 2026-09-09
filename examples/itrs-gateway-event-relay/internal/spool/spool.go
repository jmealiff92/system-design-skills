// Package spool implements the one on-disk contract shared by itrs-notify
// and event-relay: a single JSON object per line, written with one write(2)
// syscall so POSIX's PIPE_BUF atomicity guarantee holds under many
// concurrent writers (many short-lived itrs-notify processes, plus
// event-relay's own breaker-open re-spool path) with no file locking.
//
// Both binaries import this package, which is safe in Go precisely because
// it doesn't create the operational risk the equivalent choice would in
// the Python version of this design: the package is compiled *into* each
// static binary at build time, not deployed as a sibling file that could
// drift out of sync on one of 80 hosts.
package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// MaxRecordBytes must not exceed PIPE_BUF (4096 on Linux) or the
// atomic-append guarantee this package exists for stops holding.
const MaxRecordBytes = 4096

// EncodeRecord turns an already-marshaled JSON object (no trailing
// newline) into one spool/DLQ line, trimming the payload rather than
// producing a line that could exceed MaxRecordBytes and risk a torn write
// interleaved with another writer's line.
func EncodeRecord(payload []byte) []byte {
	line := make([]byte, 0, len(payload)+1)
	line = append(line, payload...)
	line = append(line, '\n')
	if len(line) <= MaxRecordBytes {
		return line
	}

	var record map[string]json.RawMessage
	if err := json.Unmarshal(payload, &record); err == nil {
		record["attributes"] = json.RawMessage(`{"_truncated":"record exceeded spool record size limit"}`)
		if trimmed, err := json.Marshal(record); err == nil {
			candidate := append(trimmed, '\n')
			if len(candidate) <= MaxRecordBytes {
				return candidate
			}
		}
	}
	// Last resort: hard-truncate. Produces an invalid JSON line, but that
	// only happens for a pathologically oversized single record, and a
	// truncated line is still inspectable by an operator.
	cut := make([]byte, MaxRecordBytes)
	copy(cut, line)
	cut[MaxRecordBytes-1] = '\n'
	return cut
}

// AtomicAppend appends line to path in a single write(2) call, creating
// the file/directory if needed.
func AtomicAppend(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}
