// Package compress wraps zstd with the settings the Python agent uses
// (zstandard.ZstdCompressor(level=3)) for classified-flow and flow-stats
// payload compression. Any conformant zstd frame is wire-compatible; the
// level only affects size.
package compress

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

var (
	defaultOnce    sync.Once
	defaultEncoder *zstd.Encoder
	defaultDecoder *zstd.Decoder
	defaultErr     error
)

func loadDefault() {
	defaultEncoder, defaultErr = zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.SpeedDefault), // klauspost's level-3 equivalent
		zstd.WithEncoderConcurrency(1),
		// Python consumers use bare ZstdDecompressor().decompress(), which
		// requires Frame_Content_Size in the header; single-segment framing
		// guarantees it at every payload size.
		zstd.WithSingleSegment(true))
	if defaultErr != nil {
		defaultErr = fmt.Errorf("creating zstd encoder: %w", defaultErr)
		return
	}
	// WithDecoderMaxMemory caps the total decompressed output at 256 MiB.
	// Legitimate flow-stats / classifier payloads are at most a few MB, so
	// this bound is never hit in normal operation.  It guards against zstd
	// decompression-bomb frames that could otherwise exhaust process memory
	// when Decode is called on untrusted input (e.g. the EE collector or
	// controller-payload paths planned for later phases).
	defaultDecoder, defaultErr = zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(256<<20))
	if defaultErr != nil {
		defaultErr = fmt.Errorf("creating zstd decoder: %w", defaultErr)
	}
}

// Encode compresses b into a single zstd frame.
func Encode(b []byte) []byte {
	defaultOnce.Do(loadDefault)
	if defaultErr != nil {
		// The fixed encoder configuration is validated by package tests and
		// cannot fail because of caller input.
		panic(defaultErr)
	}
	return defaultEncoder.EncodeAll(b, nil)
}

// Decode decompresses a single zstd frame.  The package decoder is capped at
// 256 MiB of decompressed output (zstd.WithDecoderMaxMemory) to prevent
// decompression-bomb exhaustion when called on untrusted input.  Legitimate
// flow-stats and classifier batches are well under 1 MiB, so this bound is
// never reached in normal operation.
func Decode(b []byte) ([]byte, error) {
	defaultOnce.Do(loadDefault)
	if defaultErr != nil {
		return nil, defaultErr
	}
	return defaultDecoder.DecodeAll(b, nil)
}
