package container

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// ErrOutputTruncated is returned as the terminal read error when a
// bounded output reader had to discard bytes. The bytes retained before
// the limit remain readable, and Truncated reports the same condition
// before the reader is consumed.
var ErrOutputTruncated = errors.New("container output truncated")

// TruncationReporter is implemented by readers returned from bounded
// output methods. It lets callers distinguish a short result from a
// complete result without relying on the terminal read error.
type TruncationReporter interface {
	Truncated() bool
}

// OutputStats reports the result of a streaming CLI operation. Bytes is
// the total output observed, including bytes discarded by a configured
// limit; Truncated reports whether the configured limit was exceeded.
type OutputStats struct {
	Bytes     int64
	Truncated bool
}

const boundedChunkSize = 32 * 1024

// boundedBuffer retains the first limit bytes while allowing writers to
// continue draining the child process. It is safe for os/exec's stdout
// and stderr goroutines to share one instance. Fixed-size chunks avoid
// the transient old-plus-new allocation a growing bytes.Buffer would
// need near the limit.
type boundedBuffer struct {
	mu        sync.Mutex
	chunks    [][]byte
	lengths   []int
	chunk     int
	offset    int
	retained  int64
	limit     int64
	truncated bool
}

func newBoundedBuffer(limit int64) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	original := len(p)
	for len(p) > 0 && b.retained < b.limit {
		if b.chunk >= len(b.chunks) || b.offset == len(b.chunks[b.chunk]) {
			remaining := b.limit - b.retained
			size := int64(boundedChunkSize)
			if remaining < size {
				size = remaining
			}
			b.chunks = append(b.chunks, make([]byte, int(size)))
			b.lengths = append(b.lengths, 0)
			b.chunk = len(b.chunks) - 1
			b.offset = 0
		}
		room := len(b.chunks[b.chunk]) - b.offset
		if room > len(p) {
			room = len(p)
		}
		copy(b.chunks[b.chunk][b.offset:b.offset+room], p[:room])
		b.offset += room
		b.lengths[b.chunk] = b.offset
		b.retained += int64(room)
		p = p[room:]
	}
	if len(p) > 0 {
		b.truncated = true
	}
	// Report every input byte as consumed. The child must keep running
	// after the cap so it cannot block on a full pipe.
	return original, nil
}

func (b *boundedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

func (b *boundedBuffer) reader() *boundedReader {
	b.mu.Lock()
	defer b.mu.Unlock()
	return &boundedReader{chunks: b.chunks, lengths: b.lengths, truncated: b.truncated}
}

type boundedReader struct {
	chunks    [][]byte
	lengths   []int
	chunk     int
	offset    int
	truncated bool
}

func (r *boundedReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.chunk < len(r.chunks) {
		length := r.lengths[r.chunk]
		if r.offset == length {
			r.chunk++
			r.offset = 0
			continue
		}
		n := copy(p, r.chunks[r.chunk][r.offset:length])
		r.offset += n
		if r.chunk == len(r.chunks)-1 && r.offset == length && r.truncated {
			return n, ErrOutputTruncated
		}
		return n, nil
	}
	if r.truncated {
		return 0, ErrOutputTruncated
	}
	return 0, io.EOF
}

func (r *boundedReader) Truncated() bool { return r.truncated }

type boundedReadCloser struct {
	*boundedReader
}

func (r *boundedReadCloser) Close() error { return nil }

// limitedWriter forwards at most limit bytes to dst and drains the rest.
// It is used by the streaming APIs when the caller supplied a writer but
// also requested MaxBytes.
type limitedWriter struct {
	mu        sync.Mutex
	dst       io.Writer
	limit     int64
	written   int64
	truncated bool
}

func newLimitedWriter(dst io.Writer, limit int64) *limitedWriter {
	return &limitedWriter{dst: dst, limit: limit}
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	room := w.limit - w.written
	if room <= 0 {
		if len(p) > 0 {
			w.truncated = true
		}
		return len(p), nil
	}

	keep := int64(len(p))
	if keep > room {
		keep = room
	}
	n, err := w.dst.Write(p[:int(keep)])
	if n < 0 || n > int(keep) {
		return 0, fmt.Errorf("invalid write count %d for %d-byte buffer", n, keep)
	}
	w.written += int64(n)
	// Truncation means that this wrapper intentionally discarded bytes
	// beyond its configured limit. A short/error return from the
	// destination is an output-delivery failure, not evidence that the
	// limit discarded bytes.
	if int64(len(p)) > keep {
		w.truncated = true
	}
	if err == nil && n != int(keep) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return n, err
	}
	// The remainder was intentionally discarded, not left for a later
	// write. Tell os/exec that all input was handled.
	return len(p), nil
}

func (w *limitedWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

// synchronizedWriter makes a caller-provided destination safe when the
// CLI writes stdout and stderr concurrently.
type synchronizedWriter struct {
	mu  sync.Mutex
	dst io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dst.Write(p)
}

func (w *synchronizedWriter) Truncated() bool {
	type truncationReporter interface {
		Truncated() bool
	}
	tr, ok := w.dst.(truncationReporter)
	return ok && tr.Truncated()
}

func streamWriter(dst io.Writer) io.Writer {
	if dst == nil {
		dst = io.Discard
	}
	return &synchronizedWriter{dst: dst}
}

// outputTrackingWriter records delivery failures even when a custom
// WriterRunner forgets to return an error from its Write method. The
// outer synchronized/limited writer still provides the synchronization
// and configured-limit behavior.
type outputTrackingWriter struct {
	mu  sync.Mutex
	dst io.Writer
	err error
}

func newOutputTrackingWriter(dst io.Writer) *outputTrackingWriter {
	if dst == nil {
		dst = io.Discard
	}
	return &outputTrackingWriter{dst: dst}
}

func (w *outputTrackingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	if n < 0 || n > len(p) {
		err = fmt.Errorf("invalid write count %d for %d-byte buffer", n, len(p))
	} else if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.mu.Lock()
		switch {
		case w.err == nil:
			w.err = err
		case !errors.Is(w.err, err):
			w.err = errors.Join(w.err, err)
		}
		w.mu.Unlock()
	}
	return n, err
}

func (w *outputTrackingWriter) Err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

func (w *outputTrackingWriter) Truncated() bool {
	type truncationReporter interface{ Truncated() bool }
	tr, ok := w.dst.(truncationReporter)
	return ok && tr.Truncated()
}

// preserveOutputDeliveryError adds a sink failure observed at the API
// boundary to a runner error that did not report it. This protects the
// public methods even when an injected WriterRunner violates its error
// contract, while avoiding duplicate wrappers when RunTo already joined
// the same writer error.
func preserveOutputDeliveryError(err error, tracked *outputTrackingWriter) error {
	if tracked == nil {
		return err
	}
	writerErr := tracked.Err()
	if writerErr == nil || (cli.IsOutputError(err) && errors.Is(err, writerErr)) {
		return err
	}
	deliveryErr := &cli.OutputError{Stream: "combined output", Err: writerErr}
	if err == nil {
		return deliveryErr
	}
	return errors.Join(err, deliveryErr)
}

func limitedStreamWriter(dst io.Writer, limit int64) *limitedWriter {
	if dst == nil {
		dst = io.Discard
	}
	return newLimitedWriter(dst, limit)
}
