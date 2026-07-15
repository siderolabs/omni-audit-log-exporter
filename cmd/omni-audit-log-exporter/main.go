// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main implements the entrypoint of the Omni audit log exporter.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/siderolabs/go-api-signature/pkg/serviceaccount"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/siderolabs/omni-audit-log-exporter/internal/exporter"
	"github.com/siderolabs/omni-audit-log-exporter/internal/version"
)

const envEndpoint = "OMNI_ENDPOINT"

var rootCmdArgs struct {
	omniAPIEndpoint       string
	serviceAccountKeyFile string
	stateFile             string
	startFrom             string
	insecureSkipTLSVerify bool
	debug                 bool
}

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:     version.Name,
	Short:   "Export the audit log of an Omni instance",
	Version: version.Tag,
	Args:    cobra.NoArgs,
	// the returned error is printed once by main, keeping stderr free of duplicates
	SilenceErrors: true,
	PersistentPreRun: func(cmd *cobra.Command, _ []string) {
		cmd.SilenceUsage = true // if the args are parsed fine, no need to show usage
	},
	RunE: func(cmd *cobra.Command, _ []string) error {
		logger, err := initLogger()
		if err != nil {
			return fmt.Errorf("failed to create logger: %w", err)
		}

		defer logger.Sync() //nolint:errcheck

		return run(cmd.Context(), logger)
	},
}

// initLogger builds a logger writing to stderr, so that stdout stays reserved for the
// exported events.
func initLogger() (*zap.Logger, error) {
	var loggerConfig zap.Config

	if rootCmdArgs.debug {
		loggerConfig = zap.NewDevelopmentConfig()
		loggerConfig.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		loggerConfig.Level.SetLevel(zap.DebugLevel)
	} else {
		loggerConfig = zap.NewProductionConfig()
		loggerConfig.Level.SetLevel(zap.InfoLevel)
	}

	return loggerConfig.Build(zap.AddStacktrace(zapcore.FatalLevel)) // only print stack traces for fatal errors
}

func run(ctx context.Context, logger *zap.Logger) error {
	if rootCmdArgs.omniAPIEndpoint == "" {
		return fmt.Errorf("no Omni endpoint provided: set --omni-api-endpoint or the %s environment variable", envEndpoint)
	}

	serviceAccountKey, err := loadServiceAccountKey(rootCmdArgs.serviceAccountKeyFile)
	if err != nil {
		return err
	}

	// validate the key material locally to fail fast on malformed input: runtime
	// authentication failures are retried with backoff instead
	if _, err = serviceaccount.Decode(serviceAccountKey); err != nil {
		return fmt.Errorf("invalid service account key: %w", err)
	}

	omniClient, err := client.New(
		rootCmdArgs.omniAPIEndpoint,
		client.WithServiceAccount(serviceAccountKey),
		client.WithInsecureSkipTLSVerify(rootCmdArgs.insecureSkipTLSVerify),
	)
	if err != nil {
		return fmt.Errorf("failed to create Omni client: %w", err)
	}

	defer omniClient.Close() //nolint:errcheck

	auditLogExporter, err := exporter.New(omniClient.Management(), exporter.Options{
		Output:    os.Stdout,
		StateFile: rootCmdArgs.stateFile,
		StartFrom: rootCmdArgs.startFrom,
	}, logger)
	if err != nil {
		return err
	}

	return auditLogExporter.Run(ctx)
}

// loadServiceAccountKey loads the service account key from the given file or from the
// environment.
func loadServiceAccountKey(keyFile string) (string, error) {
	if keyFile != "" {
		content, err := os.ReadFile(keyFile)
		if err != nil {
			return "", fmt.Errorf("failed to read the service account key file: %w", err)
		}

		return strings.TrimSpace(string(content)), nil
	}

	if key := os.Getenv(serviceaccount.OmniServiceAccountKeyEnvVar); key != "" {
		return key, nil
	}

	return "", fmt.Errorf("no service account key provided: set --omni-service-account-key-file or the %s environment variable",
		serviceaccount.OmniServiceAccountKeyEnvVar)
}

func main() {
	if err := runCmd(); err != nil {
		log.Fatalf("failed to run: %v", err)
	}
}

func runCmd() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()

	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.Flags().StringVar(&rootCmdArgs.omniAPIEndpoint, "omni-api-endpoint", os.Getenv(envEndpoint),
		"The endpoint of the Omni API. If not set, defaults to the "+envEndpoint+" environment variable.")
	rootCmd.Flags().StringVar(&rootCmdArgs.serviceAccountKeyFile, "omni-service-account-key-file", "",
		"File containing the base64-encoded Omni service account key. When not set, the key is read from the "+
			serviceaccount.OmniServiceAccountKeyEnvVar+" environment variable.")
	rootCmd.Flags().BoolVar(&rootCmdArgs.insecureSkipTLSVerify, "insecure-skip-tls-verify", false,
		"Skip TLS verification when connecting to the Omni API.")
	rootCmd.Flags().StringVar(&rootCmdArgs.stateFile, "state-file", "",
		"File persisting the export position across restarts. When not set, every start begins at --start-from.")
	rootCmd.Flags().StringVar(&rootCmdArgs.startFrom, "start-from", "beginning",
		`Where the export starts when there is no state: "beginning" for everything the server retains, `+
			`"now" for new events only, or an RFC3339 time.`)
	rootCmd.Flags().BoolVar(&rootCmdArgs.debug, "debug", false, "Enable debug mode & logs.")
}
