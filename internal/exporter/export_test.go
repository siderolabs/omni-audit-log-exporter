// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package exporter

import (
	"context"
	"time"
)

// SetBackoffBounds shrinks the backoff between reconnection attempts to keep the tests fast.
func (exporter *Exporter) SetBackoffBounds(minBackoff, maxBackoff time.Duration) {
	exporter.minBackoff = minBackoff
	exporter.maxBackoff = maxBackoff
}

// FollowOnce exposes a single follow iteration to the tests, so that its progress report,
// the signal resetting the backoff, can be asserted directly.
func (exporter *Exporter) FollowOnce(ctx context.Context) (bool, error) {
	return exporter.followOnce(ctx)
}

// CheckpointEvents exposes the event-count checkpoint threshold to the tests.
const CheckpointEvents = checkpointEvents
