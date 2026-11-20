package mikvformat

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

/* OGG-Opus segmentation for MikV audio encoding 0x01 (EncodingOGGOpus).

The audio track is ONE ordinary OGG-Opus stream. SplitOGGOpus cuts it into
one chunk per video frame by rewriting only the PAGE boundaries: every
packet keeps its bytes, its order and its granule timeline, each emitted
page fits one frame's time window, and the OpusHead/OpusTags pages pass
through byte-identical. Concatenating the chunks in file order therefore
reproduces a valid OGG-Opus stream - an Ogg demuxer (a firmware
ogg_pages/opus decoder, ffmpeg, ...) consumes the chunks as a plain
sequential byte stream and needs no MikV knowledge beyond "feed the audio
chunk bytes in order".

Input constraint, checked not assumed: the stream must carry uniform Opus
frames of the duration OGGOpusFrameDurationMs picks for the fps - exactly
what `ffmpeg -c:a libopus -frame_duration D` produces. Uniformity is read
from the container's own page-granule arithmetic (every non-final audio
page must end exactly at packets*span granules past the first packet's
slot; only the very last packet may be short). Both preroll conventions
are accepted: ffmpeg cancels its encoder delay out of the pts, so its
granules run clean from 1920; strict encoders bake the preroll into the
granules instead. Anything else fails loudly. */

// OGGOpusSampleRate is the granule clock every Ogg Opus stream uses: Ogg
// granules for Opus are always 48 kHz units regardless of the encoder's
// input rate (RFC 7845), and MikV declares 48000 in the header for this
// audio encoding so the frame-window arithmetic is explicit.
const OGGOpusSampleRate = 48000

// opusPacketSpans are the legal Opus frame durations in granules, longest
// first: 60/40/20/10/5/2.5 ms.
var opusPacketSpans = []int{2880, 1920, 960, 480, 240, 120}

// OGGOpusPacketSpan picks the Opus packet duration (in granules) for a
// video fps: the longest legal duration that fits one video frame window
// (48000/fps granules), which lands the interleaving at about one Opus
// packet per video frame. 24 fps -> 1920 (40 ms; 25 packets per 24
// frames), 25 fps -> 40 ms exactly 1:1, 50 fps -> 20 ms exactly 1:1.
// Errors when fps does not divide 48000 - a MikV frame window must be an
// exact granule count.
func OGGOpusPacketSpan(fps uint8) (int, error) {
	if fps == 0 || OGGOpusSampleRate%int(fps) != 0 {
		return 0, fmt.Errorf("OGG-Opus audio requires an fps that divides %d (got %d)", OGGOpusSampleRate, fps)
	}
	window := OGGOpusSampleRate / int(fps)
	for _, span := range opusPacketSpans {
		if span <= window {
			return span, nil
		}
	}
	return 0, fmt.Errorf("OGG-Opus audio: no legal Opus frame duration fits a %d-granule window", window)
}

// OGGOpusFrameDurationMs is the -frame_duration value an encoder must use
// so SplitOGGOpus accepts the resulting stream (milliseconds, e.g. 40 or
// 2.5).
func OGGOpusFrameDurationMs(fps uint8) (float64, error) {
	span, err := OGGOpusPacketSpan(fps)
	if err != nil {
		return 0, err
	}
	return float64(span) / 48.0, nil
}

// JoinOGGChunks reassembles the per-frame audio chunks of an
// EncodingOGGOpus MikV back into the OGG-Opus stream.
func JoinOGGChunks(chunks [][]byte) []byte {
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	out := make([]byte, 0, total)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

// ---------------------------------------------------------------------------
// Split

// opusPacket is one completed Opus packet with its end granule.
type opusPacket struct {
	data []byte
	end  int64
}

// SplitOGGOpus cuts one OGG-Opus byte stream into exactly frames chunks,
// chunk k holding the pages whose packets complete within frame k's window
// ((k+1)*48000/fps granules). Packets never reorder and never span
// chunks; a chunk can be empty (encoder preroll shifts the first packets,
// or the schedule simply gives a frame none). Chunk 0 starts with the
// untouched OpusHead/OpusTags pages.
//
// Packets that complete past the last frame window are dropped: the container
// has no picture to play them against, so the audio track is exactly as long
// as the frames it carries - the same budget the raw-PCM path cuts to. Concat
// the chunks and you get a valid, shorter .ogg, not the original stream.
func SplitOGGOpus(ogg []byte, fps uint8, frames int) ([][]byte, error) {
	if frames <= 0 {
		return nil, fmt.Errorf("SplitOGGOpus: frames must be > 0")
	}
	span, err := OGGOpusPacketSpan(fps)
	if err != nil {
		return nil, err
	}
	window := OGGOpusSampleRate / int(fps)

	header, serial, maxSeq, packets, err := parseOGGOpus(ogg, span)
	if err != nil {
		return nil, err
	}

	// Schedule every packet into its completion window (monotonic). A packet
	// that completes past the last window is audio with no picture: it is
	// dropped rather than squeezed into the final frame.
	perFrame := make([][]opusPacket, frames)
	for _, pkt := range packets {
		k := int((pkt.end+int64(window)-1)/int64(window)) - 1 // ceil(end/window)-1
		if k < 0 {
			k = 0
		}
		if k >= frames {
			continue
		}
		perFrame[k] = append(perFrame[k], pkt)
	}

	// Emit pages, one frame's packets per page run (page runs only when a
	// frame's packets would overflow the 255-entry segment table).
	chunks := make([][]byte, frames)
	seq := maxSeq + 1
	lastAudioFrame := -1
	for k := 0; k < frames; k++ {
		if len(perFrame[k]) > 0 {
			lastAudioFrame = k
		}
	}
	for k := 0; k < frames; k++ {
		if len(perFrame[k]) == 0 {
			continue
		}
		page, err := buildOggPageRun(serial, &seq, perFrame[k], k == lastAudioFrame)
		if err != nil {
			return nil, err
		}
		chunks[k] = page
	}
	if len(header) > 0 {
		chunks[0] = append(header, chunks[0]...)
	}
	return chunks, nil
}

// ---------------------------------------------------------------------------
// Page parsing

// parseOGGOpus walks the stream page by page, verifying every Ogg CRC-32,
// and returns: the header pages' raw bytes (the leading granule <= 0
// pages: OpusHead, OpusTags), the stream serialno, the highest page
// sequence number seen, and every completed audio packet in order
// (cross-page continuation stitched) tagged with its true end granule.
// Every audio page's granule must equal start-offset + completed-packets
// x span past the first packet's slot - that is what a uniform span-granule
// packet stream looks like; only the very last packet of the stream may
// land short (the encoder's flush), and a preroll baked into the granules
// (strict RFC 7845 encoders) is accepted as an offset of the slot grid.
func parseOGGOpus(ogg []byte, span int) (header []byte, serial uint32, maxSeq uint32, packets []opusPacket, err error) {
	if len(ogg) < 27 {
		return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: shorter than one Ogg page header")
	}
	serial = binary.LittleEndian.Uint32(ogg[14:18])
	headerDone := false
	headerOpen := false // a header packet (big OpusTags) still continuing
	var carry []byte    // audio packet continued across pages

	var base int64       // granule slot of packet 0 minus one span
	var cumulative int64 // audio packets completed so far
	var prevGran int64   // granule of the previous audio page
	base = -1

	for off := 0; off < len(ogg); {
		if len(ogg)-off < 27 {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: truncated page header at offset %d", off)
		}
		if string(ogg[off:off+4]) != "OggS" {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: bad capture pattern at offset %d", off)
		}
		if ogg[off+4] != 0 {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: unsupported Ogg version %d", ogg[off+4])
		}
		granule := int64(binary.LittleEndian.Uint64(ogg[off+6 : off+14]))
		pageSerial := binary.LittleEndian.Uint32(ogg[off+14 : off+18])
		seq := binary.LittleEndian.Uint32(ogg[off+18 : off+22])
		storedCRC := binary.LittleEndian.Uint32(ogg[off+22 : off+26])
		if pageSerial != serial {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: multiple logical Ogg streams are not supported")
		}
		if seq > maxSeq {
			maxSeq = seq
		}

		nseg := int(ogg[off+26])
		segOff := off + 27
		if len(ogg)-segOff < nseg {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: truncated segment table at offset %d", off)
		}
		segs := ogg[segOff : segOff+nseg]
		bodyOff := segOff + nseg
		bodyLen := 0
		for _, v := range segs {
			bodyLen += int(v)
		}
		if len(ogg)-bodyOff < bodyLen {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: truncated page body at offset %d", off)
		}
		pageEnd := bodyOff + bodyLen
		raw := ogg[off:pageEnd]

		// CRC check per the Ogg spec: the field itself is zeroed for the computation.
		cp := make([]byte, len(raw))
		copy(cp, raw)
		cp[22], cp[23], cp[24], cp[25] = 0, 0, 0, 0
		if oggCRC32(cp) != storedCRC {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: Ogg page at offset %d has a bad CRC", off)
		}

		if !headerDone {
			if granule <= 0 {
				// RFC 7845: the first page of an Ogg Opus stream holds
				// only the OpusHead packet. Reject anything else (a Vorbis
				// stream must not be mistaken for MikV opus audio).
				if off == 0 {
					if bodyLen < 8 || !bytes.Equal(ogg[bodyOff:bodyOff+8], []byte("OpusHead")) {
						return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: first Ogg page is not an OpusHead packet")
					}
				}
				header = append(header, raw...)
				headerOpen = nseg > 0 && segs[nseg-1] == 255
				off = pageEnd
				continue
			}
			headerDone = true
		}

		// Walk packets through the segment table, completing this page's.
		var pagePkts [][]byte
		pos := bodyOff
		i := 0
		if headerOpen {
			// Leading segments complete a header packet (already captured
			// in header raw bytes); consume them without producing an
			// audio packet.
			for i < nseg {
				v := segs[i]
				pos += int(v)
				i++
				if v < 255 {
					headerOpen = false
					break
				}
			}
			if headerOpen {
				off = pageEnd
				continue
			}
		}
		for i < nseg {
			start := pos
			for i < nseg && segs[i] == 255 {
				pos += 255
				i++
			}
			if i < nseg {
				pos += int(segs[i])
				i++
				pkt := append(carry, ogg[start:pos]...)
				carry = nil
				pagePkts = append(pagePkts, pkt)
			} else {
				// Page ended inside a packet; it completes on a later one.
				carry = append(carry, ogg[start:pos]...)
			}
		}

		// Granule arithmetic for this page against the uniform grid.
		completed := int64(len(pagePkts))
		cumBefore := cumulative
		cumulative += completed
		isFinal := pageEnd >= len(ogg)

		if base < 0 {
			// First audio page fixes the slot grid: preroll (if the
			// encoder baked one in) is the difference to the grid.
			base = granule - cumulative*int64(span)
			if isFinal && base < 0 {
				base = 0 // whole stream on one page, last packet short
			}
			if base < 0 || base > OGGOpusSampleRate {
				return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: unexpected first audio granule %d for %d completed packets (span %d)", granule, completed, span)
			}
		}
		if isFinal {
			lo, hi := base+(cumulative-1)*int64(span)+1, base+cumulative*int64(span)
			if completed == 0 {
				lo, hi = prevGran, prevGran
			}
			if granule < lo || granule > hi {
				return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: final page granule %d is not within one span of packet slot %d - not a uniform %.1f ms Opus stream (encode with -frame_duration %.1f)",
					granule, base+cumulative*int64(span), float64(span)/48.0, float64(span)/48.0)
			}
		} else if granule != base+cumulative*int64(span) {
			return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: audio page at offset %d ends at granule %d, uniform grid says %d - not a uniform %.1f ms Opus stream (encode with -frame_duration %.1f)",
				off, granule, base+cumulative*int64(span), float64(span)/48.0, float64(span)/48.0)
		}

		for j, pp := range pagePkts {
			end := base + (cumBefore+int64(j)+1)*int64(span)
			if isFinal && j == len(pagePkts)-1 {
				end = granule // the flush packet may land short
			}
			packets = append(packets, opusPacket{data: pp, end: end})
		}
		if completed > 0 {
			prevGran = granule
		}
		off = pageEnd
	}

	if len(carry) > 0 {
		return nil, 0, 0, nil, fmt.Errorf("SplitOGGOpus: stream ends inside an unfinished packet")
	}
	return header, serial, maxSeq, packets, nil
}

// ---------------------------------------------------------------------------
// Page building

// buildOggPageRun serialises one frame's packets into one or more valid
// Ogg pages (a run is split only where the 255-entry segment table would
// overflow - never inside a packet). The final page of the whole stream
// gets the EOS flag.
func buildOggPageRun(serial uint32, seq *uint32, pkts []opusPacket, isFinal bool) ([]byte, error) {
	var out []byte
	var segs []byte
	var body []byte
	var pageEnd int64

	flush := func(final bool) {
		if len(segs) == 0 {
			return
		}
		flags := byte(0)
		if final {
			flags = 0x04
		}
		out = append(out, encodeOggPage(serial, *seq, flags, pageEnd, segs, body)...)
		*seq++
		segs = segs[:0]
		body = body[:0]
	}

	for _, pkt := range pkts {
		// lacing: floor(len/255) full 255-byte segments plus one
		// terminating segment (< 255; exactly 0 for a 255-multiple packet).
		full := len(pkt.data) / 255
		rem := len(pkt.data) % 255
		need := full + 1
		if need > 255 {
			return nil, fmt.Errorf("buildOggPageRun: packet of %d bytes cannot be laced into one Ogg page", len(pkt.data))
		}
		if len(segs)+need > 255 {
			flush(false)
		}
		for j := 0; j < full; j++ {
			segs = append(segs, 255)
		}
		segs = append(segs, byte(rem))
		body = append(body, pkt.data...)
		pageEnd = pkt.end
	}
	flush(isFinal)
	return out, nil
}

// encodeOggPage writes one page (header + segment table + body) with a
// correct Ogg CRC-32.
func encodeOggPage(serial, seq uint32, flags byte, granule int64, segs, body []byte) []byte {
	page := make([]byte, 27+len(segs)+len(body))
	copy(page[0:4], "OggS")
	page[4] = 0
	page[5] = flags
	binary.LittleEndian.PutUint64(page[6:14], uint64(granule))
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	// page[22:26] crc left zero for the computation
	page[26] = byte(len(segs))
	copy(page[27:27+len(segs)], segs)
	copy(page[27+len(segs):], body)
	crc := oggCRC32(page)
	binary.LittleEndian.PutUint32(page[22:26], crc)
	return page
}

// ---------------------------------------------------------------------------
// Ogg CRC-32 (xiph): poly 0x04c11db7, MSB-first, init 0, no final xor.

var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := 0; i < 256; i++ {
		c := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if c&0x80000000 != 0 {
				c = (c << 1) ^ 0x04c11db7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return t
}()

func oggCRC32(data []byte) uint32 {
	crc := uint32(0)
	for _, b := range data {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}
