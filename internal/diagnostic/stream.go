package diagnostic

import "io"

const (
	// DefaultStreamOverlap keeps enough unprocessed input for a value or
	// structural assignment split across reads. It is deliberately finite:
	// readers may deliver arbitrarily large lines.
	DefaultStreamOverlap = 64 * 1024
	// MaxStreamOverlap bounds the memory retained by a streaming redactor.
	// Structural and explicitly registered values are protected within this
	// window; callers handling values larger than the window should use a
	// bounded source and avoid placing unbounded secrets in one diagnostic.
	MaxStreamOverlap = 1024 * 1024
	streamChunkSize  = 32 * 1024
)

type streamProcessor struct {
	redactor *Redactor
	overlap  int
	pending  []byte
	limit    int
	output   []byte
	done     bool
	emit     func([]byte)
}

func newStreamProcessor(r *Redactor, limit int, emit func([]byte)) *streamProcessor {
	if r == nil {
		r = NewRedactor()
	}
	return &streamProcessor{
		redactor: r,
		overlap:  r.streamOverlap(),
		limit:    limit,
		emit:     emit,
	}
}

func (r *Redactor) streamOverlap() int {
	// Keep a full bounded structural window by default. This is still
	// constant memory, and prevents a long password/cookie/Auth value from
	// being partially emitted when it crosses a read boundary.
	overlap := MaxStreamOverlap
	if r != nil {
		for _, entry := range r.hashes {
			if entry.length > overlap {
				overlap = entry.length
			}
		}
		for _, value := range r.values {
			if len(value) > overlap {
				overlap = len(value)
			}
		}
	}
	if overlap > MaxStreamOverlap {
		overlap = MaxStreamOverlap
	}
	return overlap
}

func (p *streamProcessor) write(input []byte) {
	if p.done {
		return
	}
	for len(input) > 0 {
		n := len(input)
		if n > streamChunkSize {
			n = streamChunkSize
		}
		p.pending = append(p.pending, input[:n]...)
		input = input[n:]
		p.process(false)
	}
}

func (p *streamProcessor) writeString(input string) {
	if p.done {
		return
	}
	for len(input) > 0 {
		n := len(input)
		if n > streamChunkSize {
			n = streamChunkSize
		}
		p.pending = append(p.pending, input[:n]...)
		input = input[n:]
		p.process(false)
	}
}

func (p *streamProcessor) process(final bool) {
	if p.done {
		return
	}
	keep := p.overlap
	if final {
		keep = 0
	}
	if len(p.pending) <= keep {
		return
	}
	cut := len(p.pending) - keep
	raw := string(p.pending[:cut])
	p.pending = append(p.pending[:0], p.pending[cut:]...)
	p.emitSafe(raw)
}

func (p *streamProcessor) emitSafe(raw string) {
	if raw == "" {
		return
	}
	safe := []byte(p.redactor.Text(raw))
	if p.limit > 0 && len(p.output) >= p.limit {
		p.done = true
		p.pending = nil
		return
	}
	if p.limit > 0 {
		remaining := p.limit - len(p.output)
		if len(safe) > remaining {
			safe = safe[:remaining]
			p.done = true
			p.pending = nil
		}
	}
	if p.emit != nil {
		p.emit(safe)
	} else {
		p.output = append(p.output, safe...)
	}
}

func (p *streamProcessor) close() {
	if p.done {
		return
	}
	p.process(true)
	p.pending = nil
}

// StreamRedactor incrementally applies a Redactor while retaining at most a
// bounded overlap. It is useful for command stderr, where the safe copy must
// be capped but a secret may be split between writes.
type StreamRedactor struct {
	processor *streamProcessor
}

// NewStreamRedactor returns an incremental redactor whose String method
// contains at most limit safe bytes. A non-positive limit means unbounded
// output and should only be used with a downstream bounded sink.
func (r *Redactor) NewStream(limit int) *StreamRedactor {
	return &StreamRedactor{processor: newStreamProcessor(r, limit, nil)}
}

func (s *StreamRedactor) Write(p []byte) (int, error) {
	if s == nil || s.processor == nil {
		return len(p), nil
	}
	s.processor.write(p)
	return len(p), nil
}

func (s *StreamRedactor) Close() error {
	if s != nil && s.processor != nil {
		s.processor.close()
	}
	return nil
}

func (s *StreamRedactor) String() string {
	if s == nil || s.processor == nil {
		return ""
	}
	return string(s.processor.output)
}

// RedactBounded applies a redactor to a string without first creating a
// second full-size redacted copy. The result is capped after redaction.
func (r *Redactor) RedactBounded(value string, limit int) string {
	stream := r.NewStream(limit)
	stream.processor.writeString(value)
	_ = stream.Close()
	return stream.String()
}

// RedactReader streams src through r into dst with bounded overlap and
// bounded intermediate state. A read error is returned after pending bytes
// are flushed, so terminal stream errors are not lost.
func (r *Redactor) RedactReader(dst io.Writer, src io.Reader) error {
	if dst == nil {
		dst = io.Discard
	}
	var writeErr error
	processor := newStreamProcessor(r, 0, func(data []byte) {
		if writeErr != nil {
			return
		}
		var n int
		n, writeErr = dst.Write(data)
		if writeErr == nil && n != len(data) {
			writeErr = io.ErrShortWrite
		}
	})
	buf := make([]byte, streamChunkSize)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			processor.write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			processor.close()
			if writeErr != nil {
				return writeErr
			}
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

// TailBuffer is a fixed-size byte ring. It never grows beyond limit and keeps
// the most recently written bytes.
type TailBuffer struct {
	limit int
	data  []byte
	pos   int
	full  bool
}

func NewTailBuffer(limit int) *TailBuffer {
	if limit < 0 {
		limit = 0
	}
	return &TailBuffer{limit: limit, data: make([]byte, limit)}
}

func (t *TailBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if t == nil || t.limit == 0 {
		return original, nil
	}
	for len(p) > 0 {
		space := t.limit - t.pos
		if len(p) < space {
			copy(t.data[t.pos:], p)
			t.pos += len(p)
			break
		}
		copy(t.data[t.pos:], p[:space])
		p = p[space:]
		t.pos = 0
		t.full = true
	}
	return original, nil
}

func (t *TailBuffer) String() string {
	if t == nil || t.limit == 0 {
		return ""
	}
	if !t.full {
		return string(t.data[:t.pos])
	}
	out := make([]byte, t.limit)
	copy(out, t.data[t.pos:])
	copy(out[t.limit-t.pos:], t.data[:t.pos])
	return string(out)
}

// RedactTail streams src through r into a fixed-size safe tail. It never
// retains the complete input and returns any terminal read error separately.
func (r *Redactor) RedactTail(src io.Reader, limit int) (string, error) {
	tail := NewTailBuffer(limit)
	err := r.RedactReader(tail, src)
	return tail.String(), err
}

var _ io.Writer = (*StreamRedactor)(nil)
var _ io.Writer = (*TailBuffer)(nil)
