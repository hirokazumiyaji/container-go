package diagnostic

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
)

const (
	// DefaultStreamOverlap keeps enough unprocessed input for a value or
	// structural assignment split across reads. It is deliberately finite:
	// readers may deliver arbitrarily large lines.
	DefaultStreamOverlap = 64 * 1024
	// MaxStreamOverlap bounds the rolling input window retained by a
	// streaming redactor. A configured value may delay one cut until its
	// bounded match completes, so transient state is at most two windows.
	// Explicit values and their expanded encodings must fit within this window;
	// streams reject a redactor that cannot protect them safely. Oversized
	// structural lines are discarded rather than partially emitted.
	MaxStreamOverlap         = 1024 * 1024
	streamChunkSize          = 32 * 1024
	streamMatchOverlap       = 256
	maxStreamValueCandidates = 65536
)

var errStreamValueTooLarge = errors.New("diagnostic value exceeds stream safety limit")

type streamProcessor struct {
	redactor          *Redactor
	overlap           int
	pending           []byte
	pendingStart      int64
	nextValueScan     int64
	nextShortScan     int64
	valueCandidates   []streamValueCandidate
	valueMatches      []streamValueMatch
	valueOverflow     bool
	byShortPrefix     map[uint32][]valueHash
	byLongPrefix      map[uint32][]valueHash
	limit             int
	output            []byte
	done              bool
	discard           *structuralDiscard
	structuralPending bool
	structuralTail    []byte
	configErr         error
	emit              func([]byte)
}

type streamValueCandidate struct {
	start int64
	value valueHash
}

type streamValueMatch struct {
	start int64
	end   int64
}

func newStreamProcessor(r *Redactor, limit int, emit func([]byte)) *streamProcessor {
	if r == nil {
		r = NewRedactor()
	}
	overlap := r.streamOverlap()
	p := &streamProcessor{
		redactor:      r,
		overlap:       overlap,
		limit:         limit,
		emit:          emit,
		byShortPrefix: make(map[uint32][]valueHash),
		byLongPrefix:  make(map[uint32][]valueHash),
	}
	for _, entry := range r.hashes {
		if entry.short {
			p.byShortPrefix[entry.prefix] = append(p.byShortPrefix[entry.prefix], entry)
		} else {
			p.byLongPrefix[entry.prefix] = append(p.byLongPrefix[entry.prefix], entry)
		}
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
		if p.discard == nil && !p.valueOverflow {
			p.scanValueMatches()
		}
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
		if p.discard == nil && !p.valueOverflow {
			p.scanValueMatches()
		}
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

func (p *streamProcessor) scanValueMatches() {
	pendingEnd := p.pendingStart + int64(len(p.pending))
	if p.nextShortScan < p.pendingStart {
		p.nextShortScan = p.pendingStart
	}
	if p.nextValueScan < p.pendingStart {
		p.nextValueScan = p.pendingStart
	}
	for position := p.nextShortScan; position < pendingEnd; position++ {
		local := int(position - p.pendingStart)
		entries := p.byShortPrefix[shortPrefixAtBytes(p.pending, local)]
		for _, entry := range entries {
			if !p.redactor.force && (entry.length < minimumTokenLength || local > 0 && entry.firstWord && wordByte(p.pending[local-1])) {
				continue
			}
			p.valueCandidates = append(p.valueCandidates, streamValueCandidate{start: position, value: entry})
		}
	}
	p.nextShortScan = pendingEnd

	longEnd := pendingEnd - 3
	if longEnd < p.pendingStart {
		longEnd = p.pendingStart
	}
	for position := p.nextValueScan; position < longEnd; position++ {
		local := int(position - p.pendingStart)
		entries := p.byLongPrefix[valuePrefixAtBytes(p.pending, local)]
		for _, entry := range entries {
			if !p.redactor.force && local > 0 && entry.firstWord && wordByte(p.pending[local-1]) {
				continue
			}
			p.valueCandidates = append(p.valueCandidates, streamValueCandidate{start: position, value: entry})
		}
	}
	p.nextValueScan = longEnd
	p.finalizeValueCandidates()
}

func (p *streamProcessor) finalizeValueCandidates() {
	if len(p.valueCandidates) == 0 {
		return
	}
	pendingEnd := p.pendingStart + int64(len(p.pending))
	kept := p.valueCandidates[:0]
	for _, candidate := range p.valueCandidates {
		end := candidate.start + int64(candidate.value.length)
		if end > pendingEnd {
			kept = append(kept, candidate)
			continue
		}
		local := int(candidate.start - p.pendingStart)
		if local < 0 || local+candidate.value.length > len(p.pending) {
			continue
		}
		if hashMatchesAtBytes(p.pending, local, candidate.value, p.redactor.force) {
			p.valueMatches = append(p.valueMatches, streamValueMatch{start: candidate.start, end: end})
		}
	}
	p.valueCandidates = kept
	if len(p.valueCandidates)+len(p.valueMatches) > maxStreamValueCandidates {
		p.valueOverflow = true
		p.valueCandidates = nil
		p.valueMatches = nil
	}
}

func (p *streamProcessor) safeCut(cut int) int {
	if cut <= 0 {
		return cut
	}
	absoluteCut := p.pendingStart + int64(cut)
	delayTo := func(start int64) {
		if start < absoluteCut {
			if cutStart := int(start - p.pendingStart); cutStart < cut {
				cut = cutStart
			}
		}
	}
	for _, match := range p.valueMatches {
		if match.start < absoluteCut && match.end > absoluteCut {
			delayTo(match.start)
		}
	}
	for _, candidate := range p.valueCandidates {
		end := candidate.start + int64(candidate.value.length)
		if candidate.start < absoluteCut && end > absoluteCut {
			delayTo(candidate.start)
		}
	}
	return cut
}

func (p *streamProcessor) dropValueState() {
	p.valueCandidates = nil
	p.valueMatches = nil
	p.valueOverflow = false
	p.nextValueScan = p.pendingStart
	p.nextShortScan = p.pendingStart
}

func (p *streamProcessor) advancePending(n int) {
	if n <= 0 {
		return
	}
	p.pendingStart += int64(n)
	if len(p.valueCandidates) > 0 {
		kept := p.valueCandidates[:0]
		for _, candidate := range p.valueCandidates {
			if candidate.start >= p.pendingStart {
				kept = append(kept, candidate)
			}
		}
		p.valueCandidates = kept
	}
	if len(p.valueMatches) > 0 {
		kept := p.valueMatches[:0]
		for _, match := range p.valueMatches {
			if match.end > p.pendingStart {
				kept = append(kept, match)
			}
		}
		p.valueMatches = kept
	}
}

func shortPrefixAtBytes(s []byte, start int) uint32 {
	if start >= len(s) {
		return 0
	}
	return fingerprintBytes(s[start : start+1])
}

func valuePrefixAtBytes(s []byte, start int) uint32 {
	if start >= len(s) {
		return 0
	}
	end := start + 4
	if end > len(s) {
		end = len(s)
	}
	return fingerprintBytes(s[start:end])
}

func fingerprintBytes(value []byte) uint32 {
	const (
		offset = 2166136261
		prime  = 16777619
	)
	hash := uint32(offset)
	for _, c := range value {
		hash ^= uint32(c)
		hash *= prime
	}
	return hash
}

func hashMatchesAtBytes(s []byte, start int, entry valueHash, force bool) bool {
	if entry.length > len(s)-start || (!force && entry.length < minimumTokenLength) {
		return false
	}
	if !force {
		if entry.firstWord && start > 0 && wordByte(s[start-1]) {
			return false
		}
		if entry.lastWord && start+entry.length < len(s) && wordByte(s[start+entry.length]) {
			return false
		}
	}
	return sha256.Sum256(s[start:start+entry.length]) == entry.digest
}

type discardMode uint8

const (
	discardContinuation discardMode = iota
	discardPEM
	discardBrace
	discardQuote
)

type structuralDiscard struct {
	mode      discardMode
	endMarker []byte
	depth     int
	quote     byte
	escaped   bool
	firstLine bool
	start     int
	started   bool
	tail      []byte
}

func newStructuralDiscard(line []byte) *structuralDiscard {
	if marker := pemEndMarker(line); marker != nil {
		return &structuralDiscard{mode: discardPEM, endMarker: marker, firstLine: true}
	}
	if start := cookieObjectBrace(line); start >= 0 {
		return &structuralDiscard{mode: discardBrace, depth: 1, firstLine: true, start: start}
	}
	if start, quote := sensitiveValueStart(line); start >= 0 {
		if quote != 0 {
			return &structuralDiscard{mode: discardQuote, quote: quote, firstLine: true, start: start}
		}
		return &structuralDiscard{mode: discardContinuation, firstLine: true}
	}
	for _, marker := range [][]byte{[]byte("bearer "), []byte("basic "), []byte("eyj"), []byte("--password"), []byte("--token"), []byte("--secret"), []byte("--env"), []byte("--label"), []byte("cookie:"), []byte("set-cookie:")} {
		if containsFoldASCII(line, marker) {
			return &structuralDiscard{mode: discardContinuation, firstLine: true}
		}
	}
	if hasSecretMarkerBytes(line) {
		return &structuralDiscard{mode: discardContinuation, firstLine: true}
	}
	return nil
}

func (d *structuralDiscard) consume(data []byte) (int, bool) {
	switch d.mode {
	case discardPEM:
		combined := make([]byte, 0, len(d.tail)+len(data))
		combined = append(combined, d.tail...)
		combined = append(combined, data...)
		if index := indexFoldASCII(combined, d.endMarker); index >= 0 {
			consumed := index + len(d.endMarker) - len(d.tail)
			d.tail = nil
			if consumed < 0 {
				consumed = 0
			}
			return consumed, true
		}
		keep := len(d.endMarker) - 1
		if keep > len(combined) {
			keep = len(combined)
		}
		d.tail = append(d.tail[:0], combined[len(combined)-keep:]...)
		return len(data), false
	case discardQuote:
		i := 0
		if !d.started {
			i = d.start + 1
			d.started = true
		}
		for ; i < len(data); i++ {
			c := data[i]
			if d.escaped {
				d.escaped = false
				continue
			}
			if c == '\\' {
				d.escaped = true
				continue
			}
			if c == d.quote {
				return i + 1, true
			}
		}
		return len(data), false
	case discardBrace:
		i := 0
		if !d.started {
			i = d.start
			d.started = true
		}
		for ; i < len(data); i++ {
			c := data[i]
			if d.quote != 0 {
				if d.escaped {
					d.escaped = false
				} else if c == '\\' {
					d.escaped = true
				} else if c == d.quote {
					d.quote = 0
				}
				continue
			}
			switch c {
			case '"', '\'':
				d.quote = c
			case '{':
				d.depth++
			case '}':
				d.depth--
				if d.depth == 0 {
					return i + 1, true
				}
			}
		}
		return len(data), false
	default:
		for i := 0; i < len(data); {
			lineEnd := bytes.IndexByte(data[i:], '\n')
			if lineEnd < 0 {
				return len(data), false
			}
			lineEnd += i
			if d.firstLine {
				d.firstLine = false
				i = lineEnd + 1
				continue
			}
			line := data[i:lineEnd]
			if blankOrAssignmentLine(line) {
				return i, true
			}
			i = lineEnd + 1
		}
		return len(data), false
	}
}

func blankOrAssignmentLine(line []byte) bool {
	trimmed := strings.TrimSpace(string(line))
	return trimmed == "" || looksLikeAssignmentLine(trimmed)
}

func pemEndMarker(line []byte) []byte {
	lower := strings.ToLower(string(line))
	begin := strings.Index(lower, "-----begin ")
	if begin < 0 {
		return nil
	}
	labelStart := begin + len("-----begin ")
	marker := "private key-----"
	rel := strings.Index(lower[labelStart:], marker)
	if rel < 0 {
		return nil
	}
	labelEnd := labelStart + rel
	label := strings.TrimSpace(string(line[labelStart:labelEnd]))
	return []byte("-----end " + label + "private key-----")
}

func cookieObjectBrace(line []byte) int {
	for i := 0; i+len("cookie") <= len(line); i++ {
		if i > 0 && isNameByte(line[i-1]) {
			continue
		}
		if !bytes.EqualFold(line[i:i+len("cookie")], []byte("cookie")) {
			continue
		}
		keyEnd := i + len("cookie")
		if keyEnd < len(line) && (line[keyEnd] == 's' || line[keyEnd] == 'S') {
			keyEnd++
		}
		if keyEnd < len(line) && (line[keyEnd] == '\'' || line[keyEnd] == '"') {
			keyEnd++
		}
		for keyEnd < len(line) && (line[keyEnd] == ' ' || line[keyEnd] == '\t' || line[keyEnd] == '\r' || line[keyEnd] == '\n') {
			keyEnd++
		}
		if keyEnd < len(line) && line[keyEnd] == ':' {
			keyEnd++
			for keyEnd < len(line) && (line[keyEnd] == ' ' || line[keyEnd] == '\t' || line[keyEnd] == '\r' || line[keyEnd] == '\n') {
				keyEnd++
			}
			if keyEnd < len(line) && line[keyEnd] == '{' {
				return keyEnd
			}
		}
	}
	return -1
}

func sensitiveValueStart(line []byte) (int, byte) {
	for i := 0; i < len(line); {
		if !isNameStartByte(line[i]) || i > 0 && isNameByte(line[i-1]) {
			i++
			continue
		}
		start := i
		i++
		for i < len(line) && isNameByte(line[i]) {
			i++
		}
		name := string(line[start:i])
		sep := i
		for sep < len(line) && (line[sep] == ' ' || line[sep] == '\t' || line[sep] == '\r' || line[sep] == '\n') {
			sep++
		}
		if sep >= len(line) || line[sep] != '=' && line[sep] != ':' || !isSecretName(name) && !isOpaqueName(name) {
			continue
		}
		sep++
		for sep < len(line) && (line[sep] == ' ' || line[sep] == '\t' || line[sep] == '\r' || line[sep] == '\n') {
			sep++
		}
		if sep < len(line) && (line[sep] == '\'' || line[sep] == '"') {
			return sep, line[sep]
		}
		return sep, 0
	}
	return -1, 0
}

func isNameStartByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (p *streamProcessor) process(final bool) {
	if p.done {
		return
	}
	if p.discard != nil {
		consumed, done := p.discard.consume(p.pending)
		if !done {
			p.advancePending(len(p.pending))
			p.pending = p.pending[:0]
			return
		}
		p.advancePending(consumed)
		p.pending = append(p.pending[:0], p.pending[consumed:]...)
		p.dropValueState()
		p.discard = nil
		p.structuralPending = false
		p.structuralTail = nil
	}
	if p.valueOverflow {
		p.discard = &structuralDiscard{mode: discardContinuation, firstLine: true}
		p.advancePending(len(p.pending))
		p.pending = nil
		p.dropValueState()
		return
	}
	if final {
		raw := string(p.pending)
		p.advancePending(len(p.pending))
		p.pending = nil
		p.dropValueState()
		p.emitSafe(raw)
		return
	}

	lastNewline := bytes.LastIndexByte(p.pending, '\n')
	partialLine := len(p.pending) - lastNewline - 1
	if partialLine > p.overlap {
		line := p.pending[lastNewline+1:]
		guard := newStructuralDiscard(line)
		if guard == nil && p.structuralPending {
			if prior := newStructuralDiscard(p.pending); prior != nil && prior.mode == discardPEM {
				guard = prior
			}
		}
		if p.structuralPending || guard != nil {
			if guard == nil {
				guard = &structuralDiscard{mode: discardContinuation, firstLine: true}
			}
			consumed, done := guard.consume(line)
			if done {
				suffix := line[consumed:]
				p.advancePending(len(p.pending) - len(suffix))
				p.pending = append(p.pending[:0], suffix...)
				p.dropValueState()
				p.structuralPending = false
				p.structuralTail = nil
				return
			}
			p.discard = guard
			p.advancePending(len(p.pending))
			p.pending = nil
			p.dropValueState()
			p.structuralPending = false
			p.structuralTail = nil
			return
		}
		cut := p.safeCut(len(p.pending) - p.overlap)
		if cut <= 0 {
			return
		}
		raw := string(p.pending[:cut])
		p.advancePending(cut)
		p.pending = append(p.pending[:0], p.pending[cut:]...)
		p.emitSafe(raw)
		p.structuralPending = false
		return
	}
	if lastNewline < 0 {
		return
	}
	cut := p.safeCut(len(p.pending) - p.overlap)
	if cut <= 0 {
		return
	}
	if p.structuralPending {
		p.discard = &structuralDiscard{mode: discardContinuation, firstLine: true}
		p.advancePending(len(p.pending))
		p.pending = nil
		p.dropValueState()
		p.structuralPending = false
		p.structuralTail = nil
		return
	}
	raw := string(p.pending[:cut])
	p.advancePending(cut)
	p.pending = append(p.pending[:0], p.pending[cut:]...)
	p.emitSafe(raw)
}

func oversizedStructuralLine(line []byte) bool {
	return newStructuralDiscard(line) != nil
}

func indexFoldASCII(s, substr []byte) int {
	if len(substr) == 0 {
		return 0
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if bytes.EqualFold(s[i:i+len(substr)], substr) {
			return i
		}
	}
	return -1
}

func containsFoldASCII(s, substr []byte) bool {
	return indexFoldASCII(s, substr) >= 0
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
	p.discard = nil
	p.dropValueState()
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
