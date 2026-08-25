// Package version holds the Conflux gateway's version identity, shared by
// the entry point's startup log line and the /_status endpoint so the version
// string has a single source of truth.
package version

// Version is the Conflux gateway version. It is a var (not a const) so the
// release build can override it at link time via -ldflags -X.
var Version = "3.0"
