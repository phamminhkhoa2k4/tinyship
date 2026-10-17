package docker

// Docker's container log stream is multiplexed when the container has no
// TTY: every chunk arrives as an 8-byte header — [0]=1 stdout|2 stderr,
// [1..3] zero, [4..7] big-endian uint32 payload length — followed by exactly
// that many payload bytes. Protocol knowledge stays inside the
// wrapper, so callers (worker log tail, log server, CLI) only ever see
// plain text. TTY containers and already-decoded bodies pass through byte
// for byte.

import (
	"bufio"
	"encoding/binary"
	"io"
)

// NewLogDemux wraps r so callers read plain text. Framed chunks are stripped
// as they stream, which keeps `logs --follow` responsive: no buffering of
// the whole stream, and stderr frames merge into the same output, so the
// reader never has to distinguish the two streams.
func NewLogDemux(r io.Reader) io.Reader { return &demuxReader{src: bufio.NewReader(r)} }

type demuxReader struct {
	src         *bufio.Reader
	rest        []byte // unread payload of the frame being delivered
	passthrough bool   // stream is unframed; stop looking for headers
}

func (d *demuxReader) Read(p []byte) (int, error) {
	for len(d.rest) == 0 {
		if d.passthrough {
			return d.src.Read(p)
		}
		hdr, err := d.src.Peek(8)
		if err != nil {
			// Fewer than 8 bytes remain: either a truncated tail of a
			// framed stream or a short final line of plain text. Flush
			// whatever is left; a real transport error surfaces from
			// the Read below.
			return d.src.Read(p)
		}
		if !isFrameHeader(hdr) {
			// Not framed (TTY container, or a body we already
			// decoded): copy through and stop sniffing.
			d.passthrough = true
			return d.src.Read(p)
		}
		_, _ = d.src.Discard(8)
		size := int(binary.BigEndian.Uint32(hdr[4:8]))
		if size == 0 {
			continue
		}
		frame := make([]byte, size)
		n, rerr := io.ReadFull(d.src, frame)
		if n > 0 {
			d.rest = frame[:n] // keep a truncated tail; EOF follows
			break
		}
		if rerr != nil {
			return 0, rerr // EOF at a frame boundary: normal close
		}
	}
	n := copy(p, d.rest)
	d.rest = d.rest[n:]
	return n, nil
}

// DemuxBytes strips Docker stream framing from an in-memory log chunk. A
// truncated final frame keeps whatever payload arrived; unframed bytes pass
// through unchanged.
func DemuxBytes(b []byte) []byte {
	var out []byte
	for len(b) >= 8 && isFrameHeader(b) {
		size := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if size > len(b) {
			size = len(b) // truncated tail: keep what arrived
		}
		out = append(out, b[:size]...)
		b = b[size:]
	}
	return append(out, b...)
}

// isFrameHeader reports whether b starts a Docker stream frame: stream id
// 1..3 in byte 0 followed by three zero bytes (and b has 8 bytes).
func isFrameHeader(b []byte) bool {
	return len(b) >= 8 && b[0] >= 1 && b[0] <= 3 && b[1] == 0 && b[2] == 0 && b[3] == 0
}
