// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package exporter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
)

// readState reads the resume position from the state file: the id of the last processed
// response as a decimal number. A missing file is a fresh start, while anything unreadable
// is an error, never silently treated as a fresh start.
func readState(path string) (int64, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}

		return 0, false, fmt.Errorf("failed to read the state file: %w", err)
	}

	position, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("malformed state file %q: %w", path, err)
	}

	if position < 0 {
		return 0, false, fmt.Errorf("malformed state file %q: negative position %d", path, position)
	}

	return position, true, nil
}

// writeState writes the resume position atomically: a temporary file in the same directory,
// synced to disk, then renamed over the state file, so that a crash mid-write can never
// leave a truncated position behind. The directory is deliberately not synced: a power cut
// losing the rename only reverts to an older position, which replays events within the
// at-least-once contract.
func writeState(path string, position int64) error {
	tmpPath := path + ".tmp"

	if err := writeFileSynced(tmpPath, strconv.FormatInt(position, 10)+"\n"); err != nil {
		os.Remove(tmpPath) //nolint:errcheck

		return err
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath) //nolint:errcheck

		return err
	}

	return nil
}

func writeFileSynced(path, content string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	defer file.Close() //nolint:errcheck // the sync below already surfaces the write errors

	if _, err = file.WriteString(content); err != nil {
		return err
	}

	return file.Sync()
}
