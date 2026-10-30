// Content hashing for the scanner and for cleanup's re-verification step.
//
// A "hash" here is a SHA-256 fingerprint of a file's bytes: two files with the
// same SHA-256 are, for all practical purposes, byte-for-byte identical (no
// accidental collision has ever been found). SHA-256 comes from Go's standard
// library and uses the CPU's hardware SHA instructions where available, so on
// most machines the disk, not the hash, is the bottleneck.
package scan

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// partialChunk is how much of the start and of the end of a file the quick
// "partial hash" stage reads (64 KiB each). Files that share a size but differ
// usually differ in their headers or trailers, so this weeds out most false
// candidates without reading whole multi-gigabyte files.
const partialChunk = 64 << 10

// ctxReader wraps a file so that a long read stops promptly when the scan is
// cancelled (Ctrl+C, or the user closing the review page). Without it,
// hashing a 50 GB video could not be interrupted until it finished.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

// Read passes through to the wrapped reader unless the context (Go's
// cancellation signal, passed down from the command) has been cancelled.
func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// FullHash reads the entire file at path and returns its SHA-256 as a hex
// string, plus how many bytes were read.
//
// Used by the scanner (final duplicate confirmation) and by internal/clean
// (re-checking a file right before it is removed). Read-only; never modifies
// the file.
func FullHash(ctx context.Context, path string) (string, int64, error) {
	f, err := os.Open(path) // #nosec G304 -- reading user-selected files is this tool's purpose
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	// 256 KiB buffer: large sequential reads are much faster on spinning
	// disks and network shares than the default 32 KiB.
	n, err := io.CopyBuffer(h, ctxReader{ctx, f}, make([]byte, 256<<10))
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// partialHash fingerprints only the first and last 64 KiB of a file.
//
// Returns the hash, whether that hash actually covered the WHOLE file (true
// for files of 128 KiB or less, where reading "both ends" means reading
// everything - in that case the result is the real full SHA-256 and the
// caller can skip the full-hash stage), and the bytes read.
func partialHash(ctx context.Context, path string, size int64) (hash string, whole bool, read int64, err error) {
	if size <= 2*partialChunk {
		h, n, err := FullHash(ctx, path)
		return h, true, n, err
	}
	if err := ctx.Err(); err != nil {
		return "", false, 0, err
	}
	f, err := os.Open(path) // #nosec G304 -- reading user-selected files is this tool's purpose
	if err != nil {
		return "", false, 0, err
	}
	defer f.Close()
	buf := make([]byte, partialChunk)
	h := sha256.New()
	// Head of the file.
	if _, err := io.ReadFull(f, buf); err != nil {
		return "", false, 0, err
	}
	h.Write(buf)
	// Tail of the file. ReadAt reads at an absolute offset; if the file
	// shrank since we measured it, this errors and the file is skipped.
	if _, err := f.ReadAt(buf, size-partialChunk); err != nil {
		return "", false, int64(partialChunk), err
	}
	h.Write(buf)
	return hex.EncodeToString(h.Sum(nil)), false, 2 * partialChunk, nil
}
