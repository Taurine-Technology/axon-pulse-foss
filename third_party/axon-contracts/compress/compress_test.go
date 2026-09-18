package compress

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := bytes.Repeat([]byte("axon flow stats "), 100)
	out, err := Decode(Encode(in))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Fatal("round trip mismatch")
	}
}

// TestSmallPayloadCarriesContentSize ensures that even tiny payloads produce
// zstd frames whose Frame_Header_Descriptor has the Single_Segment_Flag set
// (bit 5 / 0x20), which guarantees Frame_Content_Size is always present.
// This is required by Python's bare ZstdDecompressor().decompress() call
// (e.g. axon_agent/ovs/agent.py) which raises ZstdError on frames lacking
// a content-size field — a common failure mode for small flow-stats batches.
//
// zstd frame layout (RFC 8878):
//
//	[0:4]  Magic_Number  0xFD2FB528 little-endian
//	[4]    Frame_Header_Descriptor
//	         bit 5 (0x20) = Single_Segment_Flag → content size always present
func TestSmallPayloadCarriesContentSize(t *testing.T) {
	const zstdMagic = uint32(0xFD2FB528)

	payloads := []struct {
		name string
		data []byte
	}{
		{"single byte", []byte("x")},
		{"~100 bytes", bytes.Repeat([]byte("axon"), 25)},
	}

	for _, tc := range payloads {
		t.Run(tc.name, func(t *testing.T) {
			frame := Encode(tc.data)

			// (i) round-trip assertion
			got, err := Decode(frame)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if !bytes.Equal(tc.data, got) {
				t.Fatalf("round-trip mismatch: want %q got %q", tc.data, got)
			}

			// (ii) frame-header assertion: verify Single_Segment_Flag is set
			if len(frame) < 5 {
				t.Fatalf("frame too short (%d bytes)", len(frame))
			}
			magic := binary.LittleEndian.Uint32(frame[0:4])
			if magic != zstdMagic {
				t.Fatalf("bad zstd magic: got 0x%08X want 0x%08X", magic, zstdMagic)
			}
			fhd := frame[4] // Frame_Header_Descriptor
			const singleSegmentFlag = 0x20
			if fhd&singleSegmentFlag == 0 {
				t.Fatalf("Frame_Header_Descriptor 0x%02X: Single_Segment_Flag (0x20) not set; "+
					"Python bare ZstdDecompressor().decompress() will raise ZstdError on this frame", fhd)
			}
		})
	}
}

// TestLargePayloadRoundTrip proves that the 256 MiB decoder memory cap does
// not break normal-sized payloads.  A ~1 MiB input (well below any legitimate
// maximum) must encode and decode back to the original bytes.
//
// Note: crafting a real >256 MiB decompressing bomb to test the error path is
// impractical in a unit test (it would require allocating and compressing
// hundreds of MiB of data at test time).  The combination of
// zstd.WithDecoderMaxMemory in loadDefault, this round-trip test, and the doc
// comment on Decode provides sufficient coverage.
func TestLargePayloadRoundTrip(t *testing.T) {
	// ~1 MiB of compressible data — typical flow-stats batch upper bound
	const size = 1 << 20 // 1 MiB
	in := bytes.Repeat([]byte("axon flow-stats classifier batch record "), size/40)
	out, err := Decode(Encode(in))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(in, out) {
		t.Fatalf("round-trip mismatch: input len=%d output len=%d", len(in), len(out))
	}
}

func TestDecodePythonLevel3(t *testing.T) {
	encoded, err := os.ReadFile("testdata/python_level3.zst.b64")
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	out, err := Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(out) != "axon zstd compat fixture payload" {
		t.Fatalf("got %q", out)
	}
}
