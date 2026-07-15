// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/siderolabs/go-api-signature/pkg/serviceaccount"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadServiceAccountKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "key")
	require.NoError(t, os.WriteFile(keyFile, []byte(" file-key\n"), 0o600))

	t.Setenv(serviceaccount.OmniServiceAccountKeyEnvVar, "env-key")

	key, err := loadServiceAccountKey(keyFile)
	require.NoError(t, err)
	assert.Equal(t, "file-key", key, "the file wins over the environment, trimmed")

	key, err = loadServiceAccountKey("")
	require.NoError(t, err)
	assert.Equal(t, "env-key", key)

	t.Setenv(serviceaccount.OmniServiceAccountKeyEnvVar, "")

	_, err = loadServiceAccountKey("")
	require.ErrorContains(t, err, "no service account key provided")

	_, err = loadServiceAccountKey(filepath.Join(t.TempDir(), "missing"))
	require.ErrorContains(t, err, "failed to read the service account key file")
}
