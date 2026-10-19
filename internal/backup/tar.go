package backup

import (
	"archive/tar"
	"fmt"
	"io"
	"time"
)

// tarWriter wraps archive/tar with fixed, deterministic headers: every
// member is a regular file with mode 0600 and a zero timestamp, so two
// backups of identical content differ only where content differs.
type tarWriter struct {
	tw *tar.Writer
}

func newTarWriter(w io.Writer) *tarWriter {
	return &tarWriter{tw: tar.NewWriter(w)}
}

func (t *tarWriter) add(name string, data []byte) error {
	hdr := &tar.Header{
		Name:    name,
		Mode:    0o600,
		Size:    int64(len(data)),
		ModTime: time.Unix(0, 0),
		Format:  tar.FormatPAX,
	}
	if err := t.tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("backup: write header for %s: %w", name, err)
	}
	if _, err := t.tw.Write(data); err != nil {
		return fmt.Errorf("backup: write member %s: %w", name, err)
	}
	return nil
}

func (t *tarWriter) close() error {
	if err := t.tw.Close(); err != nil {
		return fmt.Errorf("backup: finish tar: %w", err)
	}
	return nil
}

// tarReader streams members out of archive/tar.
type tarReader struct {
	tr *tar.Reader
}

func newTarReader(r io.Reader) *tarReader {
	return &tarReader{tr: tar.NewReader(r)}
}

// next returns the next member's name and full content, or io.EOF.
func (t *tarReader) next() (string, []byte, error) {
	hdr, err := t.tr.Next()
	if err != nil {
		return "", nil, err
	}
	if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
		return "", nil, fmt.Errorf("backup: entry %q is not a regular file", hdr.Name)
	}
	data, err := io.ReadAll(t.tr)
	if err != nil {
		return "", nil, fmt.Errorf("backup: read entry %s: %w", hdr.Name, err)
	}
	// Guard against a decompression bomb: a member may not exceed the
	// archive limit (manifest sizes are checked against this later).
	if len(data) > MaxArchiveSize {
		return "", nil, fmt.Errorf("backup: member %q exceeds the size limit", hdr.Name)
	}
	return hdr.Name, data, nil
}
