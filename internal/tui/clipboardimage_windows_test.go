//go:build windows

package tui

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/png"
	"testing"
)

// buildTestDIB constructs a minimal, valid CF_DIB payload (BITMAPINFOHEADER
// + BI_RGB pixel data, no BITMAPFILEHEADER — the clipboard never includes
// one) for a 2x2 24bpp bitmap with one distinct color per corner, so
// dibToPNG's row-order (bottom-up) and channel-order (BGR) handling can
// both be checked precisely against a known expected image.
func buildTestDIB(t *testing.T) []byte {
	t.Helper()
	header := make([]byte, 40)
	binary.LittleEndian.PutUint32(header[0:4], 40)   // biSize
	binary.LittleEndian.PutUint32(header[4:8], 2)    // biWidth
	binary.LittleEndian.PutUint32(header[8:12], 2)   // biHeight (positive = bottom-up)
	binary.LittleEndian.PutUint16(header[12:14], 1)  // biPlanes
	binary.LittleEndian.PutUint16(header[14:16], 24) // biBitCount
	binary.LittleEndian.PutUint32(header[16:20], 0)  // biCompression = BI_RGB

	// Row size padded to a 4-byte boundary: 2 px * 3 bytes = 6, padded to 8.
	rowBottom := []byte{255, 0, 0, 255, 255, 255, 0, 0} // BGR: blue, white
	rowTop := []byte{0, 0, 255, 0, 255, 0, 0, 0}        // BGR: red, green

	dib := append(append([]byte{}, header...), rowBottom...)
	dib = append(dib, rowTop...)
	return dib
}

func TestDibToPNG_DecodesBottomUpBGR24bpp(t *testing.T) {
	dib := buildTestDIB(t)
	pngBytes, err := dibToPNG(dib)
	if err != nil {
		t.Fatalf("dibToPNG failed: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatalf("re-decoding dibToPNG's own output as PNG failed: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 2 || b.Dy() != 2 {
		t.Fatalf("decoded image bounds = %v, want 2x2", b)
	}

	check := func(x, y int, want color.NRGBA) {
		t.Helper()
		r, g, b, a := img.At(x, y).RGBA()
		got := color.NRGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
		if got != want {
			t.Fatalf("pixel (%d,%d) = %+v, want %+v", x, y, got, want)
		}
	}
	check(0, 0, color.NRGBA{R: 255, G: 0, B: 0, A: 255})     // top-left: red
	check(1, 0, color.NRGBA{R: 0, G: 255, B: 0, A: 255})     // top-right: green
	check(0, 1, color.NRGBA{R: 0, G: 0, B: 255, A: 255})     // bottom-left: blue
	check(1, 1, color.NRGBA{R: 255, G: 255, B: 255, A: 255}) // bottom-right: white
}

func TestDibToPNG_RejectsUnsupportedBitDepth(t *testing.T) {
	dib := buildTestDIB(t)
	binary.LittleEndian.PutUint16(dib[14:16], 8) // 8bpp — indexed, unsupported
	if _, err := dibToPNG(dib); err == nil {
		t.Fatal("dibToPNG accepted an 8bpp (indexed) bitmap, want a clear error instead of silently misdecoding it")
	}
}

func TestDibToPNG_RejectsTruncatedHeader(t *testing.T) {
	if _, err := dibToPNG([]byte{1, 2, 3}); err == nil {
		t.Fatal("dibToPNG accepted a truncated header, want an error")
	}
}

func TestDibToPNG_RejectsShortPixelData(t *testing.T) {
	dib := buildTestDIB(t)
	dib = dib[:len(dib)-4] // chop off part of the last row
	if _, err := dibToPNG(dib); err == nil {
		t.Fatal("dibToPNG accepted pixel data shorter than its own header claims, want an error")
	}
}
