package tools

import "context"

// image.go ports packages/agent/src/harness/tools/image.ts from upstream pi
// @0.82.0: sniffing raw bytes to identify a small set of image formats the
// built-in read tool can embed in a result (JPEG, PNG, GIF, WEBP, BMP). The
// magic-byte / header-field checks (isPng, isAnimatedPng, isBmp, and the
// byte-reading helpers they use) are a verbatim, byte-for-byte port of
// upstream's logic - including two deliberate exclusions upstream also
// makes: the `ff d8 ff f7` JPEG variant (JPEG2000's marker sequence, not a
// baseline/progressive JPEG this tool wants to embed) and animated PNG
// (APNG, detected via an `acTL` chunk appearing before the first `IDAT`
// chunk).
//
// Deviation from upstream: upstream's readUint32BE/readUint32LE build the
// top byte via multiplication (`* 0x1000000`) rather than a left-shift,
// because JS's `<<` operator is defined over 32-bit *signed* integers and
// `0xff << 24` would land in the sign bit and produce a negative number.
// Go's `int` is at least 32 bits and, on every platform this module targets
// (amd64/arm64), 64 bits - so `byte << 24` for a byte value (0-255) never
// approaches the sign bit and an ordinary left-shift is both simpler and
// numerically identical to upstream's workaround.
//
// encodeBase64 in upstream is a hand-rolled base64 encoder (used to embed
// image bytes as base64 in a tool result). This port does not include an
// equivalent: nothing in this task's scope (path.go, image.go,
// mutation_queue.go) consumes it, and Go's stdlib `encoding/base64` is a
// direct drop-in for whichever later task (the read tool) ends up needing
// it - so there is nothing to hand-roll and nothing to port here.

// pngSignature is the fixed 8-byte magic number every PNG file starts with.
var pngSignature = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}

// DetectSupportedImageMimeType sniffs data's leading bytes and returns the
// MIME type of a supported image format ("image/jpeg", "image/png",
// "image/gif", "image/webp", or "image/bmp"), or "" if data is not a
// recognized image, or is a recognized-but-unsupported variant (an
// `ff d8 ff f7` JPEG-family marker, or an animated PNG). Ported from
// upstream's detectSupportedImageMimeType.
func DetectSupportedImageMimeType(data []byte) string {
	if startsWithBytes(data, []byte{0xff, 0xd8, 0xff}) {
		if byteAt(data, 3) == 0xf7 {
			return ""
		}
		return "image/jpeg"
	}
	if startsWithBytes(data, pngSignature) {
		if isPng(data) && !isAnimatedPng(data) {
			return "image/png"
		}
		return ""
	}
	if startsWithASCII(data, 0, "GIF") {
		return "image/gif"
	}
	if startsWithASCII(data, 0, "RIFF") && startsWithASCII(data, 8, "WEBP") {
		return "image/webp"
	}
	if startsWithASCII(data, 0, "BM") && isBmp(data) {
		return "image/bmp"
	}
	return ""
}

// ReadImageProcessorResult is the outcome of a ReadImageProcessor call: the
// (possibly resized/re-encoded) image bytes to embed on success, or a
// human-readable Message to surface in place of the image on failure.
type ReadImageProcessorResult struct {
	// OK reports whether processing succeeded. When false, Data and
	// MimeType are unset and Message explains what happened.
	OK bool
	// Data is the processed image bytes. Set only when OK is true.
	Data []byte
	// MimeType is the MIME type of Data. Set only when OK is true.
	MimeType string
	// Hints are optional human-readable notes about the processing (e.g.
	// "resized from 4032x3024 to 1024x768") to surface alongside the image.
	Hints []string
	// Message explains why processing failed. Set only when OK is false.
	Message string
}

// ReadImageProcessor is an optional hook the read tool (a later task) can be
// configured with to resize/re-encode image bytes before they're embedded in
// a tool result - e.g. downscaling oversized images or converting a
// supported-but-not-directly-embeddable format. This release defines the
// seam type only; no default implementation ships, and a read tool without
// one configured embeds sniffed bytes as-is.
type ReadImageProcessor func(ctx context.Context, data []byte, mimeType string, autoResize bool) (ReadImageProcessorResult, error)

// isPng reports whether data is well-formed enough to trust as a PNG beyond
// the signature check: at least 16 bytes long (signature + first chunk's
// length and type fields), with the first chunk's declared length equal to
// 13 (the fixed size of an IHDR chunk's data) and type "IHDR". Ported
// verbatim from upstream's isPng.
func isPng(data []byte) bool {
	return len(data) >= 16 && readUint32BE(data, len(pngSignature)) == 13 && startsWithASCII(data, 12, "IHDR")
}

// isAnimatedPng walks data's chunk sequence starting after the signature and
// reports true if an "acTL" (animation control) chunk appears before the
// first "IDAT" (image data) chunk - the standard APNG detection heuristic,
// since acTL must precede IDAT in a well-formed APNG. Walking stops (false)
// once IDAT is seen, once the chunk sequence runs off the end of data, or on
// any chunk-length overflow/corruption that would otherwise loop forever.
// Ported verbatim from upstream's isAnimatedPng.
func isAnimatedPng(data []byte) bool {
	offset := len(pngSignature)
	for offset+8 <= len(data) {
		chunkLength := readUint32BE(data, offset)
		chunkTypeOffset := offset + 4
		if startsWithASCII(data, chunkTypeOffset, "acTL") {
			return true
		}
		if startsWithASCII(data, chunkTypeOffset, "IDAT") {
			return false
		}
		nextOffset := offset + 8 + chunkLength + 4
		if nextOffset <= offset || nextOffset > len(data) {
			return false
		}
		offset = nextOffset
	}
	return false
}

// isBmp validates the BMP file header and DIB (device-independent bitmap)
// header fields well enough to distinguish a real BMP from arbitrary bytes
// that happen to start with "BM": a plausible declared file size, a pixel
// data offset consistent with the header sizes, a supported DIB header size
// (12-byte BITMAPCOREHEADER, or 40-124-byte BITMAPINFOHEADER-family), a
// single color plane, and a bits-per-pixel value BMP actually supports.
// Ported verbatim from upstream's isBmp.
func isBmp(data []byte) bool {
	if len(data) < 26 {
		return false
	}
	declaredFileSize := readUint32LE(data, 2)
	pixelDataOffset := readUint32LE(data, 10)
	dibHeaderSize := readUint32LE(data, 14)
	if declaredFileSize != 0 && declaredFileSize < 26 {
		return false
	}
	if pixelDataOffset < 14+dibHeaderSize {
		return false
	}
	if declaredFileSize != 0 && pixelDataOffset >= declaredFileSize {
		return false
	}

	var colorPlanes, bitsPerPixel int
	switch {
	case dibHeaderSize == 12:
		colorPlanes = readUint16LE(data, 22)
		bitsPerPixel = readUint16LE(data, 24)
	case dibHeaderSize >= 40 && dibHeaderSize <= 124:
		if len(data) < 30 {
			return false
		}
		colorPlanes = readUint16LE(data, 26)
		bitsPerPixel = readUint16LE(data, 28)
	default:
		return false
	}
	if colorPlanes != 1 {
		return false
	}
	switch bitsPerPixel {
	case 1, 4, 8, 16, 24, 32:
		return true
	default:
		return false
	}
}

// byteAt returns data[offset], or 0 if offset is out of bounds - mirroring
// upstream's `buffer[offset] ?? 0` (JS's out-of-bounds array access yields
// undefined, coalesced to 0), which is what lets the checks above run
// unconditionally against short/truncated buffers instead of needing an
// explicit bounds check at every access.
func byteAt(data []byte, offset int) byte {
	if offset < 0 || offset >= len(data) {
		return 0
	}
	return data[offset]
}

// readUint16LE reads a 2-byte little-endian unsigned integer from data at
// offset (out-of-bounds bytes read as 0 via byteAt). Ported from upstream's
// readUint16LE.
func readUint16LE(data []byte, offset int) int {
	return int(byteAt(data, offset)) | int(byteAt(data, offset+1))<<8
}

// readUint32BE reads a 4-byte big-endian unsigned integer from data at
// offset (out-of-bounds bytes read as 0 via byteAt). Ported from upstream's
// readUint32BE (see the package doc comment for why this port uses a plain
// left-shift where upstream multiplies).
func readUint32BE(data []byte, offset int) int {
	return int(byteAt(data, offset))<<24 | int(byteAt(data, offset+1))<<16 | int(byteAt(data, offset+2))<<8 | int(byteAt(data, offset+3))
}

// readUint32LE reads a 4-byte little-endian unsigned integer from data at
// offset (out-of-bounds bytes read as 0 via byteAt). Ported from upstream's
// readUint32LE (see the package doc comment for why this port uses a plain
// left-shift where upstream multiplies).
func readUint32LE(data []byte, offset int) int {
	return int(byteAt(data, offset)) | int(byteAt(data, offset+1))<<8 | int(byteAt(data, offset+2))<<16 | int(byteAt(data, offset+3))<<24
}

// startsWithBytes reports whether data begins with the exact byte sequence
// prefix. Ported from upstream's startsWith (renamed to avoid colliding with
// the standard idiom `strings.HasPrefix`-style naming while staying
// unambiguous about comparing raw bytes rather than ASCII text).
func startsWithBytes(data []byte, prefix []byte) bool {
	if len(data) < len(prefix) {
		return false
	}
	for i, b := range prefix {
		if data[i] != b {
			return false
		}
	}
	return true
}

// startsWithASCII reports whether data contains text's ASCII bytes starting
// at offset. Ported from upstream's startsWithAscii.
func startsWithASCII(data []byte, offset int, text string) bool {
	if len(data) < offset+len(text) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if data[offset+i] != text[i] {
			return false
		}
	}
	return true
}
