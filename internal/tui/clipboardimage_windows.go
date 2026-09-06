//go:build windows

package tui

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"reflect"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// cfDIB (CF_DIB = 8) is the standard "device-independent bitmap" clipboard
// format every Windows screenshot tool (Snipping Tool, Win+Shift+S,
// PrtScn, and every browser's own "copy image") populates alongside
// whatever richer formats it also offers — the same reasoning
// github.com/atotto/clipboard's own Windows implementation (already a
// dependency, see writeClipboard) picks CF_UNICODETEXT for text: the one
// format virtually everything populates, not the richest one only some
// sources do.
const cfDIB = 8

var (
	user32Clip                     = syscall.NewLazyDLL("user32.dll")
	procIsClipboardFormatAvailable = user32Clip.NewProc("IsClipboardFormatAvailable")
	procOpenClipboard              = user32Clip.NewProc("OpenClipboard")
	procCloseClipboard             = user32Clip.NewProc("CloseClipboard")
	procGetClipboardData           = user32Clip.NewProc("GetClipboardData")

	kernel32Clip     = syscall.NewLazyDLL("kernel32.dll")
	procGlobalLock   = kernel32Clip.NewProc("GlobalLock")
	procGlobalUnlock = kernel32Clip.NewProc("GlobalUnlock")
	procGlobalSize   = kernel32Clip.NewProc("GlobalSize")
)

// waitOpenClipboard mirrors github.com/atotto/clipboard's own retry loop —
// OpenClipboard fails transiently if another process holds the clipboard
// open at the same instant, common enough (background clipboard-history
// tools, etc.) to be worth a short retry rather than failing outright.
func waitOpenClipboard() error {
	limit := time.Now().Add(time.Second)
	for time.Now().Before(limit) {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timed out opening clipboard")
}

// platformReadClipboardImage reads CF_DIB off the Windows clipboard via
// direct user32/kernel32 syscalls (no cgo — same LazyDLL/NewProc technique
// github.com/atotto/clipboard already uses for text, so this doesn't
// introduce a new cross-compilation concern beyond what's already proven
// to work in this codebase's release pipeline) and re-encodes it as PNG,
// since PNG is the format an ACP image content block actually needs, not
// the DIB is the OS gives back.
func platformReadClipboardImage() ([]byte, string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if avail, _, _ := procIsClipboardFormatAvailable.Call(cfDIB); avail == 0 {
		return nil, "", ErrNoClipboardImage
	}
	if err := waitOpenClipboard(); err != nil {
		return nil, "", err
	}
	defer procCloseClipboard.Call()

	h, _, _ := procGetClipboardData.Call(cfDIB)
	if h == 0 {
		return nil, "", ErrNoClipboardImage
	}
	size, _, _ := procGlobalSize.Call(h)
	if size == 0 {
		return nil, "", ErrNoClipboardImage
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		return nil, "", errors.New("GlobalLock failed reading clipboard bitmap")
	}
	defer procGlobalUnlock.Call(h)

	// Building the byte slice via reflect.SliceHeader rather than
	// unsafe.Slice(( *byte)(unsafe.Pointer(ptr)), ...) is deliberate, not
	// stylistic: ptr is a uintptr that never round-tripped through a real
	// Go pointer (it's a raw address handed back by a syscall), and
	// `go vet`'s unsafeptr check specifically flags a direct
	// uintptr->unsafe.Pointer conversion in that shape (case (3) in
	// unsafe.Pointer's own safety rules is the ONLY uintptr-conversion
	// case it accepts, and it requires pointer arithmetic that started
	// from a real Pointer — not applicable here). Manually filling a
	// SliceHeader's Data/Len/Cap fields is the long-established idiom
	// `go vet` explicitly special-cases as safe for exactly this "wrap a
	// syscall-returned address as a byte slice" scenario.
	var raw []byte
	sh := (*reflect.SliceHeader)(unsafe.Pointer(&raw))
	sh.Data = ptr
	sh.Len = int(size)
	sh.Cap = int(size)
	dib := make([]byte, len(raw))
	copy(dib, raw)

	data, err := dibToPNG(dib)
	if err != nil {
		return nil, "", err
	}
	return data, "image/png", nil
}

// dibToPNG decodes a raw CF_DIB payload (a BITMAPINFOHEADER followed
// directly by pixel data — the clipboard never includes the 14-byte
// BITMAPFILEHEADER a real .bmp FILE would have) into an image.Image and
// re-encodes it as PNG. Deliberately handles only the overwhelmingly
// common clipboard-screenshot shape — uncompressed (BI_RGB) or
// BI_BITFIELDS 24/32bpp, top-down or bottom-up, no color palette — the
// same "handle the real case, error clearly instead of guessing at the
// rest" approach the rest of this codebase takes for genuinely
// unverified territory (see CLAUDE.md's "Known limitations"): an indexed/
// paletted DIB is vanishingly unlikely to come from a screenshot or a
// browser's "copy image," and returning a clear error for one beats
// silently rendering garbage.
func dibToPNG(dib []byte) ([]byte, error) {
	const headerLen = 40 // sizeof(BITMAPINFOHEADER)
	if len(dib) < headerLen {
		return nil, errors.New("clipboard bitmap header is truncated")
	}
	biSize := binary.LittleEndian.Uint32(dib[0:4])
	width := int(int32(binary.LittleEndian.Uint32(dib[4:8])))
	rawHeight := int32(binary.LittleEndian.Uint32(dib[8:12]))
	bitCount := binary.LittleEndian.Uint16(dib[14:16])
	compression := binary.LittleEndian.Uint32(dib[16:20])

	if compression != 0 && compression != 3 {
		return nil, fmt.Errorf("clipboard bitmap uses an unsupported compression mode (%d)", compression)
	}
	if bitCount != 24 && bitCount != 32 {
		return nil, fmt.Errorf("clipboard bitmap has an unsupported bit depth (%d bpp) — only 24/32bpp are handled", bitCount)
	}
	if width <= 0 {
		return nil, errors.New("clipboard bitmap has an invalid width")
	}

	topDown := rawHeight < 0
	height := int(rawHeight)
	if topDown {
		height = -height
	}
	if height <= 0 {
		return nil, errors.New("clipboard bitmap has an invalid height")
	}

	dataOffset := int(biSize)
	if compression == 3 { // BI_BITFIELDS: three 4-byte channel masks follow the header
		dataOffset += 12
	}
	bytesPerPixel := int(bitCount) / 8
	rowSize := ((width*int(bitCount) + 31) / 32) * 4 // rows are padded to a 4-byte boundary
	if need := dataOffset + rowSize*height; len(dib) < need {
		return nil, fmt.Errorf("clipboard bitmap data is shorter than its own header claims (have %d bytes, need %d)", len(dib), need)
	}

	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		srcRow := y
		if !topDown {
			srcRow = height - 1 - y // DIB rows are stored bottom-up by default
		}
		rowStart := dataOffset + srcRow*rowSize
		for x := 0; x < width; x++ {
			px := rowStart + x*bytesPerPixel
			b, g, r := dib[px], dib[px+1], dib[px+2]
			img.SetNRGBA(x, y, color.NRGBA{R: r, G: g, B: b, A: 255})
		}
	}

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
