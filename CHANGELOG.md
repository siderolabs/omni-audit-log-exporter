## [omni-audit-log-exporter 0.1.0](https://github.com/siderolabs/omni-audit-log-exporter/releases/tag/v0.1.0) (2026-07-27)

Welcome to the v0.1.0 release of omni-audit-log-exporter!



Please try out the release binaries and report any issues at
https://github.com/siderolabs/omni-audit-log-exporter/issues.

### Omni Audit Log Exporter

The exporter follows the audit log of an Omni instance and writes each event to stdout as a line of JSON, so the output can be piped into any log shipper or file. It resumes where it left off across reconnects and restarts by keeping a position file, and retries transient failures with backoff. It requires an Omni service account with the Admin role, and an Omni version whose management API supports following the audit log.


### Contributors

* Utku Ozdemir

### Changes
<details><summary>3 commits</summary>
<p>

* [`bc31036`](https://github.com/siderolabs/omni-audit-log-exporter/commit/bc310364d1d2b36b3376bf78fea63e59a3f71f70) chore: do rekres to get image signing in makefile
* [`f439d26`](https://github.com/siderolabs/omni-audit-log-exporter/commit/f439d267297293117e94ac210d9b02cf9af0d0eb) feat: implement the Omni audit log exporter
* [`8c0c680`](https://github.com/siderolabs/omni-audit-log-exporter/commit/8c0c6809b9d9607dc290464f42d7f02b9afcef04) chore: bootstrap the project
</p>
</details>

### Dependency Changes

This release has no dependency changes

