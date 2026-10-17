package docker

// Demux tests: the framing contract above, proven with the shapes the
// daemon actually produces (single frame, interleaved stdout/stderr frames,
// truncated tail, and unframed TTY output) — including slow readers that
// cross frame boundaries byte by byte.

import (
	"io"
	"strings"
	"testing"
)

// frame builds a Docker stream frame: stream id 1 (stdout) or 2 (stderr).
func frame(stream byte, payload string) []byte {
	b := make([]byte, 8+len(payload))
	b[0] = stream
	binary32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

func binary32(dst []byte, v uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

func TestDemuxSingleFrame(t *testing.T) {
	in := frame(1, "hello\n")
	if got := string(DemuxBytes(in)); got != "hello\n" {
		t.Errorf("DemuxBytes = %q, want %q", got, "hello\n")
	}
	out, err := io.ReadAll(NewLogDemux(strings.NewReader(string(in))))
	if err != nil {
		t.Fatalf("streaming read: %v", err)
	}
	if string(out) != "hello\n" {
		t.Errorf("streaming demux = %q, want %q", out, "hello\n")
	}
}

func TestDemuxMergesStdoutAndStderrFrames(t *testing.T) {
	in := string(frame(1, "out1\n")) + string(frame(2, "err1\n")) + string(frame(1, "out2\n"))
	want := "out1\nerr1\nout2\n"
	if got := string(DemuxBytes([]byte(in))); got != want {
		t.Errorf("DemuxBytes = %q, want %q", got, want)
	}
	out, err := io.ReadAll(NewLogDemux(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("streaming read: %v", err)
	}
	if string(out) != want {
		t.Errorf("streaming demux = %q, want %q", out, want)
	}
}

func TestDemuxPassesUnframedTextThrough(t *testing.T) {
	// TTY containers emit raw text; a body that merely starts with a
	// plausible length must still survive byte for byte.
	plain := "plain output\n\x01\x00\x00\x00\x00\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00 tail"
	if got := string(DemuxBytes([]byte(plain))); got != plain {
		t.Errorf("DemuxBytes mangled unframed text: %q", got)
	}
	out, err := io.ReadAll(NewLogDemux(strings.NewReader(plain)))
	if err != nil {
		t.Fatalf("streaming read: %v", err)
	}
	if string(out) != plain {
		t.Errorf("streaming demux mangled unframed text: %q", out)
	}
}

func TestDemuxKeepsTruncatedFinalFrame(t *testing.T) {
	// A follow stream cut mid-frame must not lose the bytes already
	// delivered for that frame.
	in := string(frame(1, "complete\n")) + string(frame(1, "cut off mid-frame"))[:8+7]
	want := "complete\ncut off"
	if got := string(DemuxBytes([]byte(in))); got != want {
		t.Errorf("DemuxBytes = %q, want %q", got, want)
	}
	out, err := io.ReadAll(NewLogDemux(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("streaming read: %v", err)
	}
	if string(out) != want {
		t.Errorf("streaming demux = %q, want %q", out, want)
	}
}

func TestDemuxSlowReaderCrossesFrameBoundaries(t *testing.T) {
	in := string(frame(1, "AAAA")) + string(frame(2, "BBBB")) + "tail-text"
	out := NewLogDemux(strings.NewReader(in))
	var got strings.Builder
	buf := make([]byte, 3) // deliberately smaller than frames and headers
	for {
		n, err := out.Read(buf)
		got.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if got.String() != "AAAABBBBtail-text" {
		t.Errorf("slow read = %q, want %q", got.String(), "AAAABBBBtail-text")
	}
}

func TestDemuxZeroLengthFramesAreSkipped(t *testing.T) {
	in := string(frame(1, "")) + string(frame(1, "after-empty\n"))
	out, err := io.ReadAll(NewLogDemux(strings.NewReader(in)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(out) != "after-empty\n" {
		t.Errorf("demux = %q, want %q", out, "after-empty\n")
	}
}

func TestDemuxEmptyAndShortInput(t *testing.T) {
	if got := DemuxBytes(nil); len(got) != 0 {
		t.Errorf("DemuxBytes(nil) = %q, want empty", got)
	}
	if got := string(DemuxBytes([]byte("short"))); got != "short" {
		t.Errorf("DemuxBytes(short) = %q", got)
	}
	out, err := io.ReadAll(NewLogDemux(strings.NewReader("")))
	if err != nil || len(out) != 0 {
		t.Errorf("streaming empty = %q, %v", out, err)
	}
}
