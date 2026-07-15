# omni-audit-log-exporter

An audit log exporter for [Omni](https://github.com/siderolabs/omni).

It connects to the Omni API using a service account, follows the audit log and writes each event to its output as a line of JSON, resuming from where it left off across reconnects and restarts.

Work in progress.
