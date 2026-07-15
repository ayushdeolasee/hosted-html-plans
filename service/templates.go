// Package service implements `plans service install|uninstall|status` for
// launchd (macOS) and systemd (Linux), per plan.html §5.
//
// The unit/plist templates live in this directory as plain files so a user
// can read exactly what gets installed (plan.html §5), and are also
// embedded into the binary via go:embed so the binary is self-contained on
// a box that doesn't have the repo checked out.
package service

import _ "embed"

//go:embed launchd.plist.tmpl
var launchdTemplateSrc string

//go:embed systemd.service.tmpl
var systemdTemplateSrc string
