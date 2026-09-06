package tui

import "errors"

// ErrNoClipboardImage is returned by readClipboardImage when the OS
// clipboard simply doesn't currently hold image data — the common,
// expected case (the user copied text, or nothing) that Ctrl+V's handler
// treats as "silently do nothing," as opposed to a real read/decode
// failure worth telling the user about.
var ErrNoClipboardImage = errors.New("clipboard does not contain image data")

// readClipboardImage is a package-level function variable (same pattern
// as writeClipboard) so tests can substitute a fake — the real
// implementation reaches the actual OS clipboard via per-platform
// mechanisms (see clipboardimage_windows.go/_darwin.go/_linux.go/_other.go),
// none of which are guaranteed available in a headless test environment.
// Returns PNG-encoded bytes and its mime type ("image/png") on success.
var readClipboardImage = platformReadClipboardImage
