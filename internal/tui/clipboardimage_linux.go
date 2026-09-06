//go:build linux

package tui

import "os/exec"

// platformReadClipboardImage tries wl-paste (Wayland) then xclip (X11) —
// the two clipboard CLIs sandbox/opencode's own Linux tooling already
// assumes are the reasonable options (no pure-Go, no-cgo clipboard-image
// library exists that works across both display server protocols), and
// exactly the tools a user would already need installed for chorus's own
// text copy-on-select (writeClipboard, via github.com/atotto/clipboard) to
// work on Linux at all. Neither being installed, or the clipboard holding
// no image, both fall through to the same ErrNoClipboardImage — Ctrl+V's
// caller treats that as "silently do nothing," not an error worth a line.
//
// UNVERIFIED — this codebase was built and is being extended entirely
// from a Windows environment, so this has no live Linux confirmation
// behind it at all (neither wl-paste's nor xclip's exact image-type
// negotiation has been exercised). Flagged the same way CLAUDE.md's
// "Known limitations" section flags anything else written from documented
// behavior rather than a real run.
func platformReadClipboardImage() ([]byte, string, error) {
	if data, err := exec.Command("wl-paste", "--type", "image/png").Output(); err == nil && len(data) > 0 {
		return data, "image/png", nil
	}
	if data, err := exec.Command("xclip", "-selection", "clipboard", "-t", "image/png", "-o").Output(); err == nil && len(data) > 0 {
		return data, "image/png", nil
	}
	return nil, "", ErrNoClipboardImage
}
