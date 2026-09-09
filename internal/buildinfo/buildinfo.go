// Package buildinfo exposes values stamped into release binaries.
package buildinfo

// Version is overridden by the release build with, for example:
//
//	-ldflags "-X github.com/ayushdeolasee/hosted-html-plans/internal/buildinfo.Version=v1.2.3"
//
// Development builds deliberately remain ineligible for self-update because
// there is no unambiguous version to compare or roll forward from.
var Version = "dev"
