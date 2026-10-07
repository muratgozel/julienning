// Package version holds build metadata injected via -ldflags.
package version

// Version is set at build time: -ldflags "-X .../internal/version.Version=v1.2.3".
var Version = "dev"
