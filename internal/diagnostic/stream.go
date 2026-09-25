package diagnostic

import (
	"bytes"
	"errors"
	"io"
)

const (
	// DefaultStreamOverlap keeps enough unprocessed input for a value or
	// structural assignment split across reads. It is deliberately finite:
	// readers may deliver arbitrarily large lines.
	DefaultStreamOverlap = 64 * 1024
	// MaxStreamOverlap bounds the memory retained by a streaming redactor.
	// Explicit values and their expanded encodings must fit within this window;
	// streams reject a redactor that cannot protect them safely. Oversized
	// structural lines are discarded rather than partially emitted.
	MaxStreamOverlap   = 1024 * 1024
	streamChunkSize    = 32 * 1024
	streamMatchOverlap = 256
)

var errStreamValueTooLarge = errors.New("diagnostic value exceeds stream safety limit")

type streamProcessor struct {
	redactor           *Redactor
	overlap            int
	pending            []byte
	limit              int
	output             []byte
	done               bool
	discardingLongLine bool
	continuationLines  int
	structuralPending  bool
	structuralTail     []byte
	configErr          error
	emit               func([]byte)
}

func newStreamProcessor(r *Redactor, limit int, emit func([]byte)) *streamProcessor {
	if r == nil {
		r = NewRedactor()
	}
	overlap := r.streamOverlap()
	p := &streamProcessor{
		redactor: r,
		overlap:  overlap,
		limit:    limit,
		emit:     emit,
	}
	if overlap > MaxStreamOverlap {
		p.configErr = errStreamValueTooLarge
		p.done = true
	}
	return p
}

func (r *Redactor) streamOverlap() int {
	// Keep a full bounded structural window by default. Explicit values and
	// their encodings raise the requirement rather than being silently capped.
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
	return overlap
}

// StreamValueFits reports whether a value and every common escaped form fit in
// the bounded streaming redaction window.
func StreamValueFits(value string) bool {
	return NewRedactor(value).streamOverlap() <= MaxStreamOverlap
}

func (p *streamProcessor) write(input []byte) error {
	if p.configErr != nil {
		return p.configErr
	}
	if p.done {
		return nil
	}
	for len(input) > 0 && !p.done {
		n := len(input)
		if n > streamChunkSize {
			n = streamChunkSize
		}
		p.observe(input[:n])
		p.pending = append(p.pending, input[:n]...)
		input = input[n:]
		p.process(false)
	}
	return nil
}

func (p *streamProcessor) writeString(input string) error {
	if p.configErr != nil {
		return p.configErr
	}
	if p.done {
		return nil
	}
	for len(input) > 0 && !p.done {
		n := len(input)
		if n > streamChunkSize {
			n = streamChunkSize
		}
		p.observe([]byte(input[:n]))
		p.pending = append(p.pending, input[:n]...)
		input = input[n:]
		p.process(false)
	}
	return nil
}

func (p *streamProcessor) observe(input []byte) {
	scan := make([]byte, 0, len(p.structuralTail)+len(input))
	scan = append(scan, p.structuralTail...)
	scan = append(scan, input...)
	start := 0
	for i, c := range scan {
		if c != '\n' {
			continue
		}
		if oversizedStructuralLine(scan[start:i]) {
			p.structuralPending = true
		}
		start = i + 1
	}
	if start < len(scan) && oversizedStructuralLine(scan[start:]) {
		p.structuralPending = true
	}
	if len(scan) > streamMatchOverlap {
		scan = scan[len(scan)-streamMatchOverlap:]
	}
	p.structuralTail = append(p.structuralTail[:0], scan...)
}

func (p *streamProcessor) process(final bool) {
	if p.done {
		return
	}
	if p.discardingLongLine {
		idx := bytes.IndexByte(p.pending, '\n')
		if idx < 0 {
			p.pending = p.pending[:0]
			return
		}
		if p.continuationLines > 0 {
			p.continuationLines--
			p.pending = append(p.pending[:0], p.pending[idx+1:]...)
			return
		}
		p.discardingLongLine = false
		p.structuralPending = oversizedStructuralLine(p.pending)
	}
	if final {
		raw := string(p.pending)
		p.pending = nil
		p.emitSafe(raw)
		return
	}

	lastNewline := bytes.LastIndexByte(p.pending, '\n')
	partialLine := len(p.pending) - lastNewline - 1
	if partialLine > p.overlap {
		line := p.pending[lastNewline+1:]
		if p.structuralPending || oversizedStructuralLine(line) {
			// Never emit a prefix of a structural line that has grown beyond
			// the protected window. Drop it conservatively, plus the bounded
			// continuation region used by multiline structures.
			p.pending = p.pending[:lastNewline+1]
			p.discardingLongLine = true
			p.continuationLines = 64
			lastNewline = bytes.LastIndexByte(p.pending, '\n')
		} else {
			// An unlabelled oversized log line has no bounded end. Retain a
			// full structural window and stream its safe tail, preserving
			// useful recent diagnostics without risking a split assignment.
			cut := len(p.pending) - p.overlap
			raw := string(p.pending[:cut])
			p.pending = append(p.pending[:0], p.pending[cut:]...)
			p.emitSafe(raw)
			p.structuralPending = false
			return
		}
	}
	if lastNewline < 0 {
		return
	}
	cut := len(p.pending) - p.overlap
	if cut <= 0 {
		return
	}
	if p.structuralPending {
		// A structural value may continue across the proposed cut. Keep the
		// entire pending region private until a bounded end is observed.
		p.pending = nil
		p.discardingLongLine = true
		p.continuationLines = 64
		return
	}
	raw := string(p.pending[:cut])
	p.pending = append(p.pending[:0], p.pending[cut:]...)
	p.emitSafe(raw)
}

func oversizedStructuralLine(line []byte) bool {
	if secretShapedRE.Match(line) {
		return true
	}
	for _, marker := range [][]byte{
		[]byte("bearer "), []byte("basic "), []byte("eyj"),
		[]byte("--password"), []byte("--token"), []byte("--secret"),
		[]byte("--env"), []byte("--label"), []byte("cookie:"), []byte("set-cookie:"),
	} {
		if containsFoldASCII(line, marker) {
			return true
		}
	}
	return false
}

func containsFoldASCII(s, substr []byte) bool {
	if len(substr) == 0 {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if bytes.EqualFold(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
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

func (p *streamProcessor) close() error {
	if p.configErr != nil {
		return p.configErr
	}
	if p.done {
		return nil
	}
	for len(p.pending) > 0 {
		before := len(p.pending)
		p.process(true)
		if len(p.pending) == before {
			break
		}
	}
	p.pending = nil
	return nil
}

// StreamRedactor incrementally applies a Redactor while retaining at most a
// bounded overlap. It is useful for command stderr, where the safe copy must
// be capped but a secret may be split between writes.
type StreamRedactor struct {
	processor *streamProcessor
}

// NewStreamRedactor returns an incremental redactor whose String method
// contains at most limit safe bytes. Write and Close return an error before
// emitting output when an explicit value or encoding exceeds the bounded
// safety window. A non-positive limit means unbounded output and should only
// be used with a downstream bounded sink.
func (r *Redactor) NewStream(limit int) *StreamRedactor {
	return &StreamRedactor{processor: newStreamProcessor(r, limit, nil)}
}

func (s *StreamRedactor) Write(p []byte) (int, error) {
	if s == nil || s.processor == nil {
		return len(p), nil
	}
	if err := s.processor.write(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *StreamRedactor) Close() error {
	if s != nil && s.processor != nil {
		return s.processor.close()
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
	if stream.processor.configErr == nil {
		_ = stream.processor.writeString(value)
		_ = stream.processor.close()
		return stream.String()
	}
	// A complete value can be redacted safely without streaming. Public
	// callers of this convenience method have no error return, so redact
	// first and only then apply the output cap.
	safe := r.Text(value)
	if limit > 0 && len(safe) > limit {
		safe = safe[:limit]
	}
	return safe
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
			if err := processor.write(buf[:n]); err != nil {
				return err
			}
			if writeErr != nil {
				return writeErr
			}
		}
		if readErr != nil {
			_ = processor.close()
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
