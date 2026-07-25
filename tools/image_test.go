package tools

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// image_test.go exercises image.go, the port of
// packages/agent/src/harness/tools/image.ts @0.82.0. Test fixtures build the
// minimum bytes each format's magic-byte/header checks require, rather than
// embedding real image files, so each case pins exactly which bytes drive
// the detection decision.

// pngChunk builds one PNG chunk: a big-endian length, the 4-byte ASCII
// chunk type, the chunk data, and a 4-byte CRC placeholder (isPng and
// isAnimatedPng never validate the CRC, so its value is irrelevant here).
func pngChunk(chunkType string, data []byte) []byte {
	var buf bytes.Buffer
	length := make([]byte, 4)
	binary.BigEndian.PutUint32(length, uint32(len(data)))
	buf.Write(length)
	buf.WriteString(chunkType)
	buf.Write(data)
	buf.Write(make([]byte, 4)) // CRC, unchecked
	return buf.Bytes()
}

// buildPNG prepends the PNG signature to a sequence of chunks.
func buildPNG(chunks ...[]byte) []byte {
	var buf bytes.Buffer
	buf.Write(pngSignature)
	for _, c := range chunks {
		buf.Write(c)
	}
	return buf.Bytes()
}

// buildBMP builds a minimal-but-plausible 24bpp BMP: a 14-byte file header
// followed by a 40-byte BITMAPINFOHEADER (the standard, most common DIB
// header), plus a few bytes of trailing "pixel data" so the declared file
// size legitimately exceeds the pixel data offset (isBmp rejects a file
// whose declared size doesn't leave room for any pixel data).
func buildBMP() []byte {
	const headerSize = 14 + 40 // file header + BITMAPINFOHEADER
	const pixelBytes = 4
	const fileSize = headerSize + pixelBytes

	buf := make([]byte, fileSize)
	buf[0], buf[1] = 'B', 'M'
	binary.LittleEndian.PutUint32(buf[2:6], uint32(fileSize))     // declared file size
	binary.LittleEndian.PutUint32(buf[10:14], uint32(headerSize)) // pixel data offset
	binary.LittleEndian.PutUint32(buf[14:18], 40)                 // DIB header size (BITMAPINFOHEADER)
	binary.LittleEndian.PutUint32(buf[18:22], 1)                  // width
	binary.LittleEndian.PutUint32(buf[22:26], 1)                  // height
	binary.LittleEndian.PutUint16(buf[26:28], 1)                  // color planes (must be 1)
	binary.LittleEndian.PutUint16(buf[28:30], 24)                 // bits per pixel
	for i := headerSize; i < fileSize; i++ {
		buf[i] = 0xff // arbitrary pixel data
	}
	return buf
}

func TestDetectSupportedImageMimeType(t *testing.T) {
	minimalIHDR := make([]byte, 13)

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{
			name: "jpeg: ff d8 ff signature",
			data: []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'},
			want: "image/jpeg",
		},
		{
			name: "jpeg: ff d8 ff f7 is excluded (returns empty)",
			data: []byte{0xff, 0xd8, 0xff, 0xf7, 0x00, 0x00},
			want: "",
		},
		{
			name: "jpeg: short buffer (byte 3 out of bounds reads as 0, not 0xf7) is still jpeg",
			data: []byte{0xff, 0xd8, 0xff},
			want: "image/jpeg",
		},
		{
			name: "png: signature + minimal valid IHDR chunk",
			data: buildPNG(pngChunk("IHDR", minimalIHDR)),
			want: "image/png",
		},
		{
			name: "png: animated (acTL chunk before IDAT) is excluded (returns empty)",
			data: buildPNG(pngChunk("IHDR", minimalIHDR), pngChunk("acTL", make([]byte, 8))),
			want: "",
		},
		{
			name: "png: acTL after IDAT is NOT treated as animated",
			data: buildPNG(pngChunk("IHDR", minimalIHDR), pngChunk("IDAT", []byte{1, 2, 3}), pngChunk("acTL", make([]byte, 8))),
			want: "image/png",
		},
		{
			name: "png: signature only, no IHDR chunk, is not a valid png",
			data: pngSignature,
			want: "",
		},
		{
			name: "gif: GIF89a header",
			data: []byte("GIF89a\x01\x00\x01\x00"),
			want: "image/gif",
		},
		{
			name: "gif: GIF87a header",
			data: []byte("GIF87a"),
			want: "image/gif",
		},
		{
			name: "webp: RIFF....WEBP header",
			data: append([]byte("RIFF"), append([]byte{0x24, 0x00, 0x00, 0x00}, []byte("WEBP")...)...),
			want: "image/webp",
		},
		{
			name: "webp: RIFF without WEBP at offset 8 is not webp",
			data: append([]byte("RIFF"), append([]byte{0x24, 0x00, 0x00, 0x00}, []byte("AVI ")...)...),
			want: "",
		},
		{
			name: "bmp: BM + plausible 40-byte BITMAPINFOHEADER",
			data: buildBMP(),
			want: "image/bmp",
		},
		{
			name: "bmp: BM prefix but too short to be a real header",
			data: []byte("BM\x00\x00\x00\x00"),
			want: "",
		},
		{
			name: "truncated/garbage bytes",
			data: []byte{0x00, 0x01, 0x02, 0x03},
			want: "",
		},
		{
			name: "empty buffer",
			data: []byte{},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectSupportedImageMimeType(tc.data)
			if got != tc.want {
				t.Errorf("DetectSupportedImageMimeType(%x) = %q, want %q", tc.data, got, tc.want)
			}
		})
	}
}

func TestIsBmp(t *testing.T) {
	t.Run("dib header size 12 (BITMAPCOREHEADER) is supported", func(t *testing.T) {
		const headerSize = 14 + 12
		const fileSize = headerSize + 4
		buf := make([]byte, fileSize)
		buf[0], buf[1] = 'B', 'M'
		binary.LittleEndian.PutUint32(buf[2:6], uint32(fileSize))
		binary.LittleEndian.PutUint32(buf[10:14], uint32(headerSize))
		binary.LittleEndian.PutUint32(buf[14:18], 12) // BITMAPCOREHEADER size
		binary.LittleEndian.PutUint16(buf[22:24], 1)  // color planes
		binary.LittleEndian.PutUint16(buf[24:26], 24) // bits per pixel
		if !isBmp(buf) {
			t.Errorf("isBmp(12-byte DIB header) = false, want true")
		}
	})

	t.Run("unsupported bits-per-pixel value is rejected", func(t *testing.T) {
		buf := buildBMP()
		binary.LittleEndian.PutUint16(buf[28:30], 17) // not in {1,4,8,16,24,32}
		if isBmp(buf) {
			t.Errorf("isBmp(bitsPerPixel=17) = true, want false")
		}
	})

	t.Run("color planes != 1 is rejected", func(t *testing.T) {
		buf := buildBMP()
		binary.LittleEndian.PutUint16(buf[26:28], 2)
		if isBmp(buf) {
			t.Errorf("isBmp(colorPlanes=2) = true, want false")
		}
	})

	t.Run("declared file size at or below pixel data offset is rejected", func(t *testing.T) {
		buf := buildBMP()
		// 40 is >= 26 (passes the minimum-size check) but <= the 54-byte
		// pixel data offset buildBMP() sets up, so this exercises the
		// "pixelDataOffset >= declaredFileSize" rejection specifically,
		// not the earlier "declaredFileSize < 26" one.
		binary.LittleEndian.PutUint32(buf[2:6], 40)
		if isBmp(buf) {
			t.Errorf("isBmp(declaredFileSize <= pixelDataOffset) = true, want false")
		}
	})
}
