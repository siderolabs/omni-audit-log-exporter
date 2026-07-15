// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package exporter implements the Omni audit log exporter: it follows the audit log of an
// Omni instance and writes each event to its output, resuming from where it left off.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"math/rand/v2"
	"time"

	"github.com/siderolabs/omni/client/api/omni/management"
	managementcli "github.com/siderolabs/omni/client/pkg/client/management"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The resume position is persisted on every acknowledgment (once per stream), after this
// many events, plus once more before every reconnect and on shutdown, bounding the events
// a hard crash can replay.
const checkpointEvents = 256

// Default bounds for the backoff between reconnection attempts after a stream error.
const (
	defaultMinBackoff = time.Second
	defaultMaxBackoff = time.Minute
)

// AuditLogFollower follows the audit log of an Omni instance endlessly, yielding the
// acknowledgment of every stream along with the events. The management API client
// implements it.
type AuditLogFollower interface {
	FollowAuditLog(ctx context.Context, req *management.ReadAuditLogRequest) iter.Seq2[*management.ReadAuditLogResponse, error]
}

// Options configure the exporter.
type Options struct {
	// Output receives the exported events, one line of JSON per event.
	Output io.Writer

	// StateFile persists the resume position across restarts. Empty means no persistence:
	// every start begins at StartFrom.
	StateFile string

	// StartFrom positions the export when no state exists: "beginning" (also the default
	// when empty) for everything the server retains, "now" for new events only, or an
	// RFC3339 time.
	StartFrom string
}

// Exporter follows the audit log of an Omni instance and writes each event to its output.
type Exporter struct {
	follower AuditLogFollower
	output   io.Writer
	logger   *zap.Logger

	stateFile string

	startTsMs int64

	minBackoff time.Duration
	maxBackoff time.Duration

	// position is the id of the last processed response, valid once havePosition is set.
	// The acknowledgment opening every stream makes it valid before any event arrives.
	position int64

	// persistedPosition mirrors the state file, valid once persistedValid is set, so that
	// clean checkpoints are skipped.
	persistedPosition int64

	eventsSinceCheckpoint int

	havePosition   bool
	persistedValid bool
}

// New validates the options, loads the persisted position if there is one, and returns an
// exporter ready to run.
func New(follower AuditLogFollower, options Options, logger *zap.Logger) (*Exporter, error) {
	startTsMs, err := parseStartFrom(options.StartFrom)
	if err != nil {
		return nil, err
	}

	exporter := &Exporter{
		follower:   follower,
		output:     options.Output,
		logger:     logger,
		stateFile:  options.StateFile,
		startTsMs:  startTsMs,
		minBackoff: defaultMinBackoff,
		maxBackoff: defaultMaxBackoff,
	}

	if options.StateFile != "" {
		position, ok, err := readState(options.StateFile)
		if err != nil {
			return nil, err
		}

		if ok {
			exporter.position, exporter.havePosition = position, true
			exporter.persistedPosition, exporter.persistedValid = position, true
		}
	}

	return exporter, nil
}

// parseStartFrom maps the start position to the timestamp field of the follow request:
// one asks for everything the server retains, zero for the current tail.
func parseStartFrom(startFrom string) (int64, error) {
	switch startFrom {
	case "", "beginning":
		return 1, nil
	case "now":
		return 0, nil
	}

	parsed, err := time.Parse(time.RFC3339, startFrom)
	if err != nil {
		return 0, fmt.Errorf("invalid start position %q: expected %q, %q or an RFC3339 time: %w", startFrom, "beginning", "now", err)
	}

	startTsMs := parsed.UnixMilli()
	if startTsMs <= 0 {
		return 0, fmt.Errorf("invalid start position %q: the time must be after the Unix epoch", startFrom)
	}

	return startTsMs, nil
}

// Run follows the audit log until ctx is canceled, writing each event to the output.
//
// Stream errors are retried with backoff, resuming from the last processed position. The
// failures that retrying cannot heal end the run: the server not supporting follow, the
// server no longer knowing the resume position (a database restored from a backup), and
// failures to write an event or the state file.
func (exporter *Exporter) Run(ctx context.Context) error {
	exporter.logger.Info(
		"starting the audit log export",
		zap.Bool("resuming", exporter.havePosition),
		zap.Int64("position", exporter.position),
		zap.Int64("start_ts_ms", exporter.startTsMs),
	)

	var backoff time.Duration

	for {
		progressed, err := exporter.followOnce(ctx)

		// checkpoint before anything else: without it, a restart would replay everything
		// since the last periodic write
		persistErr := exporter.persist()

		var fatalErr *fatalError

		switch {
		case ctx.Err() != nil: // shutting down: the stream error is the cancellation surfacing
			return persistErr
		case persistErr != nil:
			return persistErr
		case errors.As(err, &fatalErr):
			return fatalErr.err
		case errors.Is(err, managementcli.ErrAuditLogFollowUnsupported):
			return err
		case status.Code(err) == codes.FailedPrecondition:
			return fmt.Errorf(
				"the server no longer knows the resume position, e.g. because its database was restored from a backup, "+
					"restart with a fresh state to recover: %w", err,
			)
		}

		if progressed {
			backoff = 0
		}

		backoff = exporter.nextBackoff(backoff)

		exporter.logger.Warn("following the audit log failed, retrying", zap.Duration("backoff", backoff), zap.Error(err))

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
	}
}

// followOnce consumes one follow iteration from the current position until it fails,
// advancing the position and checkpointing along the way. It reports whether any event was
// delivered, so that the caller resets its backoff only on real progress: a bare
// acknowledgment does not count, keeping the backoff growing against a server that fails
// right after every handshake.
func (exporter *Exporter) followOnce(ctx context.Context) (bool, error) {
	req := &management.ReadAuditLogRequest{}

	if exporter.havePosition {
		req.FromId = exporter.position + 1
	} else {
		req.StartTsMs = exporter.startTsMs
	}

	progressed := false

	for resp, err := range exporter.follower.FollowAuditLog(ctx, req) {
		if err != nil {
			return progressed, err
		}

		isEvent := len(resp.AuditLog) != 0

		if isEvent {
			// the payloads arrive newline-terminated, write them as received
			if _, err := exporter.output.Write(resp.AuditLog); err != nil {
				return progressed, &fatalError{fmt.Errorf("failed to write an event to the output: %w", err)}
			}

			exporter.eventsSinceCheckpoint++
			progressed = true
		}

		exporter.position, exporter.havePosition = resp.Id, true

		// Acknowledgments arrive once per stream and carry the resolved start position:
		// checkpoint them immediately, so that a start from the tail survives a crash
		// before the first event. Events checkpoint on the periodic thresholds.
		if err := exporter.checkpoint(!isEvent); err != nil {
			return progressed, &fatalError{err}
		}
	}

	// the iterator only ends by yielding an error, but do not depend on that here: the
	// caller backs off and reconnects
	return progressed, nil
}

// checkpoint persists the resume position when forced or when enough events accumulated
// since the last write.
func (exporter *Exporter) checkpoint(force bool) error {
	if !force && exporter.eventsSinceCheckpoint < checkpointEvents {
		return nil
	}

	return exporter.persist()
}

// persist writes the resume position to the state file, unless it is already up to date or
// there is nothing to write.
func (exporter *Exporter) persist() error {
	if exporter.stateFile == "" || !exporter.havePosition {
		return nil
	}

	if !exporter.persistedValid || exporter.persistedPosition != exporter.position {
		if err := writeState(exporter.stateFile, exporter.position); err != nil {
			return fmt.Errorf("failed to persist the resume position: %w", err)
		}

		exporter.persistedPosition, exporter.persistedValid = exporter.position, true
	}

	exporter.eventsSinceCheckpoint = 0

	return nil
}

// nextBackoff doubles the previous delay within the bounds and jitters it, so that
// followers do not reconnect in lockstep.
func (exporter *Exporter) nextBackoff(previous time.Duration) time.Duration {
	next := min(max(2*previous, exporter.minBackoff), exporter.maxBackoff)

	return next/2 + rand.N(next/2+1)
}

// fatalError marks a failure of the exporter itself, which must end the run instead of
// being retried like a stream error.
type fatalError struct {
	err error
}

func (e *fatalError) Error() string { return e.err.Error() }

func (e *fatalError) Unwrap() error { return e.err }
