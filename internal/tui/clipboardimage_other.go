//go:build !windows && !darwin && !linux

package tui

// platformReadClipboardImage has no implementation on any OS besides
// Windows/macOS/Linux — chorus's own release workflow (CLAUDE.md) only
// ever cross-compiles for those three anyway. Always reports "no image,"
// the same as a real clipboard with no image on a supported OS, so Ctrl+V
// just silently does nothing here rather than needing its own special case.
func platformReadClipboardImage() ([]byte, string, error) {
	return nil, "", ErrNoClipboardImage
}
