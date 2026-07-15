// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package exporter_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/siderolabs/omni/client/api/omni/management"
	managementcli "github.com/siderolabs/omni/client/pkg/client/management"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/siderolabs/omni-audit-log-exporter/internal/exporter"
)

// followScript is one follow iteration served by the fake follower: its responses, an
// optional check running after them, and the error ending it. A nil error ends the test
// instead, by canceling the run context.
type followScript struct {
	err       error
	check     func()
	responses []*management.ReadAuditLogResponse
}

// fakeFollower serves one scripted iteration per follow call and records the request of
// each. When it runs out of scripts, it cancels the run context, ending the test cleanly.
type fakeFollower struct {
	cancel   context.CancelFunc
	scripts  []followScript
	requests []*management.ReadAuditLogRequest
}

func (f *fakeFollower) FollowAuditLog(ctx context.Context, req *management.ReadAuditLogRequest) iter.Seq2[*management.ReadAuditLogResponse, error] {
	return func(yield func(*management.ReadAuditLogResponse, error) bool) {
		f.requests = append(f.requests, req.CloneVT())

		if len(f.scripts) == 0 {
			f.cancel()
			yield(nil, ctx.Err())

			return
		}

		script := f.scripts[0]
		f.scripts = f.scripts[1:]

		for _, resp := range script.responses {
			if !yield(resp, nil) {
				return
			}
		}

		if script.check != nil {
			script.check()
		}

		err := script.err
		if err == nil {
			f.cancel()

			err = context.Canceled
		}

		yield(nil, err)
	}
}

// ack builds the event-less acknowledgment response carrying the resolved start position.
func ack(pos int64) *management.ReadAuditLogResponse {
	return &management.ReadAuditLogResponse{Id: pos}
}

func event(id int64, payload string) *management.ReadAuditLogResponse {
	return &management.ReadAuditLogResponse{AuditLog: []byte(payload + "\n"), Id: id}
}

var errStream = status.Error(codes.Unavailable, "stream failure")

// runExporter builds an exporter over the scripted follower and runs it to completion,
// returning the follower for request assertions, everything written to the output, and the
// run error.
func runExporter(t *testing.T, options exporter.Options, scripts ...followScript) (*fakeFollower, string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	follower := &fakeFollower{cancel: cancel, scripts: scripts}

	var output bytes.Buffer

	if options.Output == nil {
		options.Output = &output
	}

	exp, err := exporter.New(follower, options, zaptest.NewLogger(t))
	require.NoError(t, err)

	exp.SetBackoffBounds(time.Millisecond, 4*time.Millisecond)

	err = exp.Run(ctx)

	return follower, output.String(), err
}

func stateFilePath(t *testing.T) string {
	t.Helper()

	return filepath.Join(t.TempDir(), "state")
}

func readStateFile(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(data)
}

func TestExportWritesEventsAndPersistsPosition(t *testing.T) {
	t.Parallel()

	stateFile := stateFilePath(t)

	follower, output, err := runExporter(
		t, exporter.Options{StateFile: stateFile},
		followScript{responses: []*management.ReadAuditLogResponse{ack(0), event(1, "a"), event(2, "b")}},
	)
	require.NoError(t, err, "running out of scripts is a clean shutdown")

	assert.Equal(t, "a\nb\n", output, "every event exactly once, one line each")
	assert.Equal(t, "2\n", readStateFile(t, stateFile), "the position of the last event survives the shutdown")
	require.Len(t, follower.requests, 1)
	assert.Equal(t, int64(1), follower.requests[0].StartTsMs, "the default start is the beginning")
	assert.Zero(t, follower.requests[0].FromId)
}

func TestExportResumesFromStateFile(t *testing.T) {
	t.Parallel()

	stateFile := stateFilePath(t)
	require.NoError(t, os.WriteFile(stateFile, []byte("41\n"), 0o644))

	follower, output, err := runExporter(
		t, exporter.Options{StateFile: stateFile},
		followScript{responses: []*management.ReadAuditLogResponse{ack(41), event(42, "a")}},
	)
	require.NoError(t, err)

	assert.Equal(t, "a\n", output)
	assert.Equal(t, "42\n", readStateFile(t, stateFile))
	require.Len(t, follower.requests, 1)
	assert.Equal(t, int64(42), follower.requests[0].FromId, "the resume is exactly after the persisted position")
	assert.Zero(t, follower.requests[0].StartTsMs, "a persisted position overrides the start position")
}

func TestExportStartPositions(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		startFrom string
		startTsMs int64
	}{
		{startFrom: "beginning", startTsMs: 1},
		{startFrom: "now", startTsMs: 0},
		{startFrom: "2026-01-02T15:04:05Z", startTsMs: time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC).UnixMilli()},
	} {
		t.Run(test.startFrom, func(t *testing.T) {
			t.Parallel()

			follower, _, err := runExporter(t, exporter.Options{StartFrom: test.startFrom})
			require.NoError(t, err)

			require.Len(t, follower.requests, 1)
			assert.Equal(t, test.startTsMs, follower.requests[0].StartTsMs)
		})
	}
}

func TestExportRejectsInvalidStartPositions(t *testing.T) {
	t.Parallel()

	for _, startFrom := range []string{"whenever", "1965-01-02T15:04:05Z"} {
		t.Run(startFrom, func(t *testing.T) {
			t.Parallel()

			_, err := exporter.New(&fakeFollower{}, exporter.Options{StartFrom: startFrom}, zaptest.NewLogger(t))
			require.ErrorContains(t, err, "invalid start position")
		})
	}
}

func TestExportRejectsCorruptStateFile(t *testing.T) {
	t.Parallel()

	stateFile := stateFilePath(t)
	require.NoError(t, os.WriteFile(stateFile, []byte("not a number\n"), 0o644))

	_, err := exporter.New(&fakeFollower{}, exporter.Options{StateFile: stateFile}, zaptest.NewLogger(t))
	require.ErrorContains(t, err, "malformed state file", "a corrupt state file must never silently become a fresh start")
}

func TestExportPersistsAcknowledgmentImmediately(t *testing.T) {
	t.Parallel()

	stateFile := stateFilePath(t)

	// the stream fails right after the acknowledgment: the resolved position must already
	// be on disk, so that a crash before the first event does not lose a tail start
	follower, output, err := runExporter(
		t, exporter.Options{StateFile: stateFile, StartFrom: "now"},
		followScript{
			responses: []*management.ReadAuditLogResponse{ack(40)},
			check:     func() { assert.Equal(t, "40\n", readStateFile(t, stateFile)) },
			err:       errStream,
		},
		followScript{responses: []*management.ReadAuditLogResponse{ack(40), event(41, "a")}},
	)
	require.NoError(t, err)

	assert.Equal(t, "a\n", output, "the event written during the failure is picked up on the retry")
	require.Len(t, follower.requests, 2)
	assert.Zero(t, follower.requests[0].FromId)
	assert.Equal(t, int64(41), follower.requests[1].FromId, "the retry resumes from the acknowledged position")
}

func TestExportRetriesStreamErrors(t *testing.T) {
	t.Parallel()

	follower, output, err := runExporter(
		t, exporter.Options{},
		followScript{err: errStream},
		followScript{err: errStream},
		followScript{responses: []*management.ReadAuditLogResponse{ack(0), event(1, "a")}},
	)
	require.NoError(t, err)

	assert.Equal(t, "a\n", output)
	assert.Len(t, follower.requests, 3, "each stream error is retried")
}

func TestExportFailsOnUnsupportedServer(t *testing.T) {
	t.Parallel()

	_, _, err := runExporter(
		t, exporter.Options{},
		followScript{err: managementcli.ErrAuditLogFollowUnsupported},
	)
	require.ErrorIs(t, err, managementcli.ErrAuditLogFollowUnsupported)
}

func TestExportFailsOnLostPosition(t *testing.T) {
	t.Parallel()

	stateFile := stateFilePath(t)
	require.NoError(t, os.WriteFile(stateFile, []byte("1000\n"), 0o644))

	_, _, err := runExporter(
		t, exporter.Options{StateFile: stateFile},
		followScript{err: status.Error(codes.FailedPrecondition, "the follow position no longer exists")},
	)
	require.ErrorContains(t, err, "no longer knows the resume position")
}

func TestExportProgressCountsEventsOnly(t *testing.T) {
	t.Parallel()

	follower := &fakeFollower{
		scripts: []followScript{
			{responses: []*management.ReadAuditLogResponse{ack(40)}, err: errStream},
			{responses: []*management.ReadAuditLogResponse{ack(40), event(41, "a")}, err: errStream},
		},
	}

	exp, err := exporter.New(follower, exporter.Options{Output: &bytes.Buffer{}}, zaptest.NewLogger(t))
	require.NoError(t, err)

	progressed, err := exp.FollowOnce(t.Context())
	require.ErrorIs(t, err, errStream)
	assert.False(t, progressed,
		"a bare acknowledgment is not progress: a server failing after every handshake must face a growing backoff")

	progressed, err = exp.FollowOnce(t.Context())
	require.ErrorIs(t, err, errStream)
	assert.True(t, progressed, "a delivered event is progress")
}

func TestExportFailsOnStatePersistError(t *testing.T) {
	t.Parallel()

	// the state file sits in a directory that does not exist: reading it is a fresh start,
	// but the first checkpoint must end the run instead of being retried
	stateFile := filepath.Join(t.TempDir(), "missing", "state")

	_, _, err := runExporter(
		t, exporter.Options{StateFile: stateFile},
		followScript{responses: []*management.ReadAuditLogResponse{ack(40)}},
	)
	require.ErrorContains(t, err, "failed to persist the resume position")
}

func TestExportFailsOnOutputError(t *testing.T) {
	t.Parallel()

	stateFile := stateFilePath(t)

	_, _, err := runExporter(
		t, exporter.Options{StateFile: stateFile, Output: &failingWriter{}},
		followScript{responses: []*management.ReadAuditLogResponse{ack(40), event(41, "a")}},
	)
	require.ErrorContains(t, err, "failed to write an event")

	assert.Equal(t, "40\n", readStateFile(t, stateFile),
		"the position must not advance past an event that was never written out")
}

func TestExportCheckpointsOnEventThreshold(t *testing.T) {
	t.Parallel()

	stateFile := stateFilePath(t)

	responses := make([]*management.ReadAuditLogResponse, 0, exporter.CheckpointEvents+1)

	responses = append(responses, ack(0))
	for i := range int64(exporter.CheckpointEvents) {
		responses = append(responses, event(i+1, "e"+strconv.FormatInt(i+1, 10)))
	}

	// the last scripted event crosses the threshold: the checkpoint must be on disk before
	// the stream ends
	_, output, err := runExporter(
		t, exporter.Options{StateFile: stateFile},
		followScript{
			responses: responses,
			check: func() {
				assert.Equal(t, fmt.Sprintf("%d\n", exporter.CheckpointEvents), readStateFile(t, stateFile))
			},
			err: errStream,
		},
		followScript{},
	)
	require.NoError(t, err)

	assert.Equal(t, exporter.CheckpointEvents, strings.Count(output, "\n"))
}

func TestExportWithoutStateFile(t *testing.T) {
	t.Parallel()

	follower, output, err := runExporter(
		t, exporter.Options{},
		followScript{responses: []*management.ReadAuditLogResponse{ack(0), event(1, "a")}, err: errStream},
		followScript{responses: []*management.ReadAuditLogResponse{ack(1), event(2, "b")}},
	)
	require.NoError(t, err)

	assert.Equal(t, "a\nb\n", output)
	require.Len(t, follower.requests, 2)
	assert.Equal(t, int64(2), follower.requests[1].FromId, "the in-memory position still drives the resume")
}

type failingWriter struct{}

func (*failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}
