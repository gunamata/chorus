//go:build darwin

package tui

import (
	"os/exec"
	"strings"
)

// platformReadClipboardImage shells out to macOS's own pbpaste/osascript —
// no image-clipboard library exists as a pure-Go, no-cgo dependency for
// macOS, and this codebase's release pipeline (CLAUDE.md) requires no cgo
// anywhere in the module graph. `clipboard info` lists every UTI/type
// currently on the pasteboard without materializing any of them, so it's
// used purely as a cheap "is there actually a picture here" check before
// asking pbpaste to materialize one — pbpaste's own `-Prefer` flag doesn't
// distinguish "clipboard has no image" from "clipboard has an image pbpaste
// couldn't convert," so checking first gives a real ErrNoClipboardImage
// instead of guessing from an empty result either way.
//
// UNVERIFIED — written from documented pbpaste/osascript behavior, not
// confirmed against a real macOS clipboard (this codebase was built and is
// being extended entirely from a Windows environment). Flagging this
// explicitly rather than silently claiming parity, the same convention
// CLAUDE.md's "Known limitations" uses elsewhere for anything that
// couldn't be live-tested where it was written.
func platformReadClipboardImage() ([]byte, string, error) {
	info, err := exec.Command("osascript", "-e", "clipboard info").Output()
	if err != nil {
		return nil, "", ErrNoClipboardImage
	}
	s := strings.ToLower(string(info))
	if !strings.Contains(s, "picture") && !strings.Contains(s, "png") && !strings.Contains(s, "tiff") {
		return nil, "", ErrNoClipboardImage
	}
	data, err := exec.Command("pbpaste", "-Prefer", "png").Output()
	if err != nil || len(data) == 0 {
		return nil, "", ErrNoClipboardImage
	}
	return data, "image/png", nil
}
