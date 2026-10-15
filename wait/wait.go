// Package wait provides startup readiness strategies for containers.
// Apple Container has no healthcheck or wait primitive, so every
// strategy here probes from the client side.
package wait

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

const (
	defaultStartupTimeout      = 60 * time.Second
	defaultPollInterval        = 100 * time.Millisecond
	stateCheckInterval         = time.Second
	lifecycleProbeTimeout      = 5 * time.Second
	finalLifecycleProbeTimeout = 30 * time.Second
)

// Target is the container surface strategies probe. *container.Container
// is adapted to it by container.Run.
type Target interface {
	// Endpoint resolves a declared container port ("6379/tcp") to a
	// dialable "host:port". An empty port asks the built-in adapter for
	// its first exposed TCP declaration, falling back to a published TCP
	// declaration.
	Endpoint(ctx context.Context, port string) (string, error)
	// Running reports whether the container is still running.
	Running(ctx context.Context) (bool, error)
	// FollowLogs streams log output; Close releases the stream. A
	// terminal CLI failure is returned by Read after the stream starts.
	FollowLogs(ctx context.Context) (io.ReadCloser, error)
	// ExecCommand runs a command in the container and returns its
	// exit code.
	ExecCommand(ctx context.Context, cmd []string) (int, error)
}

// State is a container lifecycle state reported by Target.
type State string

const (
	StateUnknown    State = "unknown"
	StateCreated    State = "created"
	StateRunning    State = "running"
	StateStopping   State = "stopping"
	StateStopped    State = "stopped"
	StateRestarting State = "restarting"
	StatePaused     State = "paused"
)

// StateTarget is an optional extension to Target for targets that can
// distinguish transitional states (Created, Restarting, Unknown) from
// terminal ones (Stopped, Stopping, Paused). Built-in strategies use it
// when available and fall back to Running.
type StateTarget interface {
	State(ctx context.Context) (State, error)
}

// Strategy waits until a started container is ready for use.
// Implementations must return when ctx is done so composite strategies can
// collect their final causes without extending the caller's budget.
type Strategy interface {
	WaitUntilReady(ctx context.Context, target Target) error
}

type options struct {
	startupTimeout time.Duration
	pollInterval   time.Duration
}

func (o options) effective() (timeout, interval time.Duration) {
	timeout, interval = o.startupTimeout, o.pollInterval
	if timeout == 0 {
		timeout = defaultStartupTimeout
	}
	if interval == 0 {
		interval = defaultPollInterval
	}
	return timeout, interval
}

// waitError keeps a conventional single context cause for errors.Unwrap
// while exposing additional transient causes through errors.Is/As.
type waitError struct {
	message string
	primary error
	causes  []error
}

func (e *waitError) Error() string { return e.message }
func (e *waitError) Unwrap() error { return e.primary }
func (e *waitError) Is(target error) bool {
	for _, cause := range e.causes {
		if errors.Is(cause, target) {
			return true
		}
	}
	return false
}
func (e *waitError) As(target any) bool {
	for _, cause := range e.causes {
		if errors.As(cause, target) {
			return true
		}
	}
	return false
}

func (e *waitError) AsRedacted(target any, redactor *cli.Redactor) bool {
	if e == nil {
		return false
	}
	for _, cause := range e.causes {
		if errors.As(cli.WithRedactor(cause, redactor), target) {
			return true
		}
	}
	return false
}

func newWaitError(message string, causes ...error) error {
	filtered := make([]error, 0, len(causes))
	for _, cause := range causes {
		if cause != nil {
			filtered = append(filtered, cause)
		}
	}
	var primary error
	if len(filtered) > 0 {
		primary = filtered[0]
	}
	return &waitError{message: message, primary: primary, causes: filtered}
}

func waitContextError(what string, contextErr, lastErr error) error {
	message := fmt.Sprintf("%s: %s", what, contextErr)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, contextErr, lastErr)
}

func waitTimeoutError(what string, timeout time.Duration, lastErr error) error {
	message := fmt.Sprintf("%s: timed out after %v", what, timeout)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, context.DeadlineExceeded, lastErr)
}

func waitStoppedStateError(what string, state State, lastErr error) error {
	message := fmt.Sprintf("%s: container %s while waiting", what, state)
	if lastErr != nil {
		message += fmt.Sprintf(" (last error: %v)", lastErr)
	}
	return newWaitError(message, lastErr)
}

func stateFailure(what string, state State, checkErr, stateErr error) error {
	return waitStoppedStateError(what, state, joinNonNil(checkErr, stateErr))
}

// targetState uses the richer optional interface when available. Running is a
// compatibility fallback whose false result necessarily means stopped.
func targetState(ctx context.Context, target Target) (State, error) {
	if stateTarget, ok := target.(StateTarget); ok {
		state, err := stateTarget.State(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if permanentProbeError(err) || isPermanentCheckError(err) {
				return StateUnknown, ctxErr
			}
			return StateUnknown, joinNonNil(err, ctxErr)
		}
		if err != nil {
			return StateUnknown, err
		}
		return canonicalState(state), nil
	}
	running, err := target.Running(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		if permanentProbeError(err) || isPermanentCheckError(err) {
			return StateUnknown, ctxErr
		}
		return StateUnknown, joinNonNil(err, ctxErr)
	}
	if err != nil {
		return StateUnknown, err
	}
	if running {
		return StateRunning, nil
	}
	return StateStopped, nil
}

func canonicalState(state State) State {
	switch state {
	case StateCreated, StateRunning, StateStopping, StateStopped, StateRestarting, StatePaused:
		return state
	default:
		return StateUnknown
	}
}

// terminalWaitState reports lifecycle states from which a container cannot
// become ready during startup without external intervention. Created,
// restarting, unknown, and transient inspect errors are retried under the
// startup timeout. Stopping includes backend removal transitions.
func terminalWaitState(state State) bool {
	return state == StateStopping || state == StateStopped || state == StatePaused
}

func waitContextTerminationError(callerCtx, waitCtx context.Context, what string, timeout time.Duration, lastErr error) error {
	if err := callerCtx.Err(); err != nil {
		return waitContextError(what, err, lastErr)
	}
	if err := waitCtx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return waitTimeoutError(what, timeout, lastErr)
		}
		return waitContextError(what, err, lastErr)
	}
	return nil
}

func finalLifecycleError(
	callerCtx, waitCtx context.Context,
	target Target,
	what string,
	timeout, interval time.Duration,
	lastErr error,
) error {
	// A successful endpoint/exec/log check can race a short-lived daemon
	// hiccup. The lifecycle check is still a fail-fast guard for a stopped
	// container, but transient probe failures get another attempt while the
	// original wait budget remains. Retries reuse the strategy's poll
	// interval so a transient daemon failure cannot turn into a faster
	// probe loop than the check it follows, and only the latest probe cause
	// is retained so retries cannot grow an error tree.
	var lastProbeErr error
	for {
		probeCtx, probeCancel := boundedProbeContext(waitCtx, lifecycleProbeTimeout)
		if err := probeCtx.Err(); err != nil {
			probeCancel()
			return waitContextTerminationError(
				callerCtx,
				waitCtx,
				what,
				timeout,
				joinNonNil(lastErr, lastProbeErr, err),
			)
		}
		state, probeErr := targetState(probeCtx, target)
		probeCtxErr := probeCtx.Err()
		probeCancel()
		if probeCtxErr != nil {
			probeErr = joinNonNil(probeErr, probeCtxErr)
		}
		// Retain only non-context branches for diagnostics/timeout wrapping,
		// but keep the raw probeErr for control flow so a pure probe-context
		// deadline still retries instead of falling through as StateUnknown.
		if filtered := nonContextCause(probeErr); filtered != nil {
			lastProbeErr = filtered
		}
		if terminalErr := waitContextTerminationError(
			callerCtx,
			waitCtx,
			what,
			timeout,
			joinNonNil(lastErr, lastProbeErr),
		); terminalErr != nil {
			return terminalErr
		}
		if probeErr != nil {
			if isPermanentCheckError(probeErr) {
				cause := permanentCause(probeErr)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, joinNonNil(lastErr, lastProbeErr))
				}
				if lastProbeErr != nil {
					return wrapWaitCause(what, lastProbeErr, joinNonNil(lastErr, lastProbeErr))
				}
				// Context-only permanent wrapper while the wait is still
				// active: retry like a transient probe failure.
			}
			if err := waitForReconnect(waitCtx, interval); err != nil {
				return waitContextTerminationError(
					callerCtx,
					waitCtx,
					what,
					timeout,
					joinNonNil(lastErr, lastProbeErr),
				)
			}
			continue
		}
		if terminalWaitState(state) {
			return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
		}
		if state != StateRunning {
			return fmt.Errorf("%s: final lifecycle state %s; want running", what, state)
		}
		return nil
	}
}

func joinNonNil(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for _, err := range errs {
		if err != nil {
			filtered = append(filtered, err)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return errors.Join(filtered...)
}

// nonContextCause drops cancellation and deadline errors so a diagnostic
// settle that races the wait budget cannot hide an already-observed
// success. Joined non-context causes, such as a terminal CLIError, remain.
func nonContextCause(err error) error {
	if err == nil {
		return nil
	}
	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		inners := unwrapper.Unwrap()
		kept := make([]error, 0, len(inners))
		changed := false
		for _, inner := range inners {
			cause := nonContextCause(inner)
			if cause == nil {
				changed = true
				continue
			}
			if !sameError(cause, inner) {
				changed = true
			}
			kept = append(kept, cause)
		}
		if len(kept) == 0 {
			return preserveEmptyContextWrapper(err)
		}
		joined := joinNonNil(kept...)
		if !changed {
			// Children are unchanged, but the wrapper itself may still
			// contribute a context match via its own Is method.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return replaceUnwrappedCause(err, joined)
			}
			return err
		}
		// Custom wrappers and fmt multi-%w errors keep framing via
		// contextStrippedCause. Plain errors.Join trees are rebuilt from the
		// filtered children so Error() no longer mentions stripped sentinels.
		if keepsClassificationWithoutChildren(err) || errorPkgPath(err) == "fmt" {
			return replaceUnwrappedCause(err, joined)
		}
		return joined
	}
	// Only unwrap a single-error wrapper when the tree contains a context
	// branch that must be stripped. Otherwise keep wrappers such as
	// *net.OpError so callers retain errors.As and operation diagnostics.
	if unwrapper, ok := err.(interface{ Unwrap() error }); ok &&
		(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		inner := unwrapper.Unwrap()
		if inner == nil {
			return preserveEmptyContextWrapper(err)
		}
		filtered := nonContextCause(inner)
		if filtered == nil {
			return preserveEmptyContextWrapper(err)
		}
		// Even when the inner value is unchanged, the wrapper may still
		// contribute a context match via its own Is method.
		return replaceUnwrappedCause(err, filtered)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Pure context sentinels have nothing else to retain.
		if err == context.Canceled || err == context.DeadlineExceeded {
			return nil
		}
		// Opaque facades (Is/As without Unwrap), such as cli.WithRedactor,
		// expose the source tree only via UnwrapRaw. Filter that tree so a
		// context-only facade is dropped while a mixed join keeps its
		// non-context surface under the original facade diagnostics.
		if raw, ok := err.(interface{ UnwrapRaw() error }); ok {
			filtered := nonContextCause(raw.UnwrapRaw())
			if filtered == nil {
				return nil
			}
			filtered = retainDiagnosticFacade(err, filtered)
			return &contextStrippedCause{original: err, cause: filtered}
		}
		// Custom Is/As facades with no unwrap surface still need their
		// non-context classifications retained after context matches are
		// stripped.
		if keepsClassificationWithoutChildren(err) {
			return &contextStrippedCause{original: err, cause: nil}
		}
		return nil
	}
	return err
}

// retainDiagnosticFacade reapplies an opaque facade's redactor onto the
// filtered cause so Error/As cannot expose secrets that only that redactor
// knew how to protect.
func retainDiagnosticFacade(facade, filtered error) error {
	type redactorProvider interface {
		DiagnosticRedactor() *cli.Redactor
	}
	if provider, ok := facade.(redactorProvider); ok {
		if r := provider.DiagnosticRedactor(); r != nil {
			return cli.WithRedactor(filtered, r)
		}
	}
	return filtered
}

// preserveEmptyContextWrapper keeps a custom wrapper's type and Is surface
// after every child was a context sentinel. Stdlib join/wrap errors and
// transport wrappers exist only to carry children, so they are dropped once
// nothing remains.
func preserveEmptyContextWrapper(err error) error {
	if !keepsClassificationWithoutChildren(err) {
		return nil
	}
	if isStdlibTransportWrapper(err) {
		return nil
	}
	return &contextStrippedCause{original: err, cause: nil}
}

func keepsClassificationWithoutChildren(err error) bool {
	path := errorPkgPath(err)
	switch path {
	case "", "errors", "fmt":
		return false
	}
	// Standard-library package paths never contain a dot. Carrier-only
	// wrappers such as os/exec.Error or strconv.NumError exist only to wrap
	// children, so once those are gone nothing independent remains.
	if isStdlibPackagePath(path) {
		return false
	}
	return true
}

func isStdlibTransportWrapper(err error) bool {
	return isStdlibPackagePath(errorPkgPath(err))
}

// stdlibPackagePaths are exact public standard-library import paths
// (from `go list std`, excluding internal/vendor). Exact membership only:
// prefix matching on roots such as "net/" would treat consumer modules like
// net/corp or text/errors as stdlib and drop their concrete As surface.
var stdlibPackagePaths = map[string]struct{}{
	"archive/tar": {},
	"archive/zip": {},
	"bufio": {},
	"bytes": {},
	"cmp": {},
	"compress/bzip2": {},
	"compress/flate": {},
	"compress/gzip": {},
	"compress/lzw": {},
	"compress/zlib": {},
	"container/heap": {},
	"container/list": {},
	"container/ring": {},
	"context": {},
	"crypto": {},
	"crypto/aes": {},
	"crypto/cipher": {},
	"crypto/des": {},
	"crypto/dsa": {},
	"crypto/ecdh": {},
	"crypto/ecdsa": {},
	"crypto/ed25519": {},
	"crypto/elliptic": {},
	"crypto/fips140": {},
	"crypto/hkdf": {},
	"crypto/hmac": {},
	"crypto/hpke": {},
	"crypto/md5": {},
	"crypto/mldsa": {},
	"crypto/mlkem": {},
	"crypto/mlkem/mlkemtest": {},
	"crypto/pbkdf2": {},
	"crypto/rand": {},
	"crypto/rc4": {},
	"crypto/rsa": {},
	"crypto/sha1": {},
	"crypto/sha256": {},
	"crypto/sha3": {},
	"crypto/sha512": {},
	"crypto/subtle": {},
	"crypto/tls": {},
	"crypto/x509": {},
	"crypto/x509/pkix": {},
	"database/sql": {},
	"database/sql/driver": {},
	"debug/buildinfo": {},
	"debug/dwarf": {},
	"debug/elf": {},
	"debug/gosym": {},
	"debug/macho": {},
	"debug/pe": {},
	"debug/plan9obj": {},
	"embed": {},
	"encoding": {},
	"encoding/ascii85": {},
	"encoding/asn1": {},
	"encoding/base32": {},
	"encoding/base64": {},
	"encoding/binary": {},
	"encoding/csv": {},
	"encoding/gob": {},
	"encoding/hex": {},
	"encoding/json": {},
	"encoding/json/jsontext": {},
	"encoding/json/v2": {},
	"encoding/pem": {},
	"encoding/xml": {},
	"errors": {},
	"expvar": {},
	"flag": {},
	"fmt": {},
	"go/ast": {},
	"go/build": {},
	"go/build/constraint": {},
	"go/constant": {},
	"go/doc": {},
	"go/doc/comment": {},
	"go/format": {},
	"go/importer": {},
	"go/parser": {},
	"go/printer": {},
	"go/scanner": {},
	"go/token": {},
	"go/types": {},
	"go/version": {},
	"hash": {},
	"hash/adler32": {},
	"hash/crc32": {},
	"hash/crc64": {},
	"hash/fnv": {},
	"hash/maphash": {},
	"html": {},
	"html/template": {},
	"image": {},
	"image/color": {},
	"image/color/palette": {},
	"image/draw": {},
	"image/gif": {},
	"image/jpeg": {},
	"image/png": {},
	"index/suffixarray": {},
	"internal/abi": {},
	"internal/asan": {},
	"internal/bisect": {},
	"internal/buildcfg": {},
	"internal/bytealg": {},
	"internal/byteorder": {},
	"internal/cfg": {},
	"internal/chacha8rand": {},
	"internal/copyright": {},
	"internal/coverage": {},
	"internal/coverage/calloc": {},
	"internal/coverage/cfile": {},
	"internal/coverage/cformat": {},
	"internal/coverage/cmerge": {},
	"internal/coverage/decodecounter": {},
	"internal/coverage/decodemeta": {},
	"internal/coverage/encodecounter": {},
	"internal/coverage/encodemeta": {},
	"internal/coverage/pods": {},
	"internal/coverage/rtcov": {},
	"internal/coverage/slicereader": {},
	"internal/coverage/slicewriter": {},
	"internal/coverage/stringtab": {},
	"internal/coverage/test": {},
	"internal/coverage/uleb128": {},
	"internal/cpu": {},
	"internal/dag": {},
	"internal/diff": {},
	"internal/exportdata": {},
	"internal/filepathlite": {},
	"internal/fmtsort": {},
	"internal/fuzz": {},
	"internal/gate": {},
	"internal/goarch": {},
	"internal/godebug": {},
	"internal/godebugs": {},
	"internal/goexperiment": {},
	"internal/goos": {},
	"internal/goroot": {},
	"internal/gover": {},
	"internal/goversion": {},
	"internal/lazyregexp": {},
	"internal/lazytemplate": {},
	"internal/msan": {},
	"internal/nettest": {},
	"internal/nettrace": {},
	"internal/obscuretestdata": {},
	"internal/oserror": {},
	"internal/pkgbits": {},
	"internal/platform": {},
	"internal/poll": {},
	"internal/profile": {},
	"internal/profilerecord": {},
	"internal/race": {},
	"internal/reflectlite": {},
	"internal/routebsd": {},
	"internal/runtime/atomic": {},
	"internal/runtime/cgobench": {},
	"internal/runtime/cgroup": {},
	"internal/runtime/exithook": {},
	"internal/runtime/gc": {},
	"internal/runtime/gc/scan": {},
	"internal/runtime/maps": {},
	"internal/runtime/math": {},
	"internal/runtime/pprof/label": {},
	"internal/runtime/sys": {},
	"internal/runtime/wasitest": {},
	"internal/saferio": {},
	"internal/singleflight": {},
	"internal/strconv": {},
	"internal/stringslite": {},
	"internal/sync": {},
	"internal/synctest": {},
	"internal/syscall/execenv": {},
	"internal/syscall/unix": {},
	"internal/sysinfo": {},
	"internal/syslist": {},
	"internal/testenv": {},
	"internal/testhash": {},
	"internal/testlog": {},
	"internal/testpty": {},
	"internal/trace": {},
	"internal/trace/raw": {},
	"internal/trace/testtrace": {},
	"internal/trace/tracev2": {},
	"internal/trace/traceviewer": {},
	"internal/trace/traceviewer/format": {},
	"internal/trace/version": {},
	"internal/txtar": {},
	"internal/types/errors": {},
	"internal/unsafeheader": {},
	"internal/xcoff": {},
	"internal/zstd": {},
	"io": {},
	"io/fs": {},
	"io/ioutil": {},
	"iter": {},
	"log": {},
	"log/slog": {},
	"log/syslog": {},
	"maps": {},
	"math": {},
	"math/big": {},
	"math/bits": {},
	"math/cmplx": {},
	"math/rand": {},
	"math/rand/v2": {},
	"mime": {},
	"mime/multipart": {},
	"mime/quotedprintable": {},
	"net": {},
	"net/http": {},
	"net/http/cgi": {},
	"net/http/cookiejar": {},
	"net/http/fcgi": {},
	"net/http/httptest": {},
	"net/http/httptrace": {},
	"net/http/httputil": {},
	"net/http/pprof": {},
	"net/mail": {},
	"net/netip": {},
	"net/rpc": {},
	"net/rpc/jsonrpc": {},
	"net/smtp": {},
	"net/textproto": {},
	"net/url": {},
	"os": {},
	"os/exec": {},
	"os/signal": {},
	"os/user": {},
	"path": {},
	"path/filepath": {},
	"plugin": {},
	"reflect": {},
	"regexp": {},
	"regexp/syntax": {},
	"runtime": {},
	"runtime/cgo": {},
	"runtime/coverage": {},
	"runtime/debug": {},
	"runtime/metrics": {},
	"runtime/pprof": {},
	"runtime/race": {},
	"runtime/trace": {},
	"slices": {},
	"sort": {},
	"strconv": {},
	"strings": {},
	"structs": {},
	"sync": {},
	"sync/atomic": {},
	"syscall": {},
	"testing": {},
	"testing/cryptotest": {},
	"testing/fstest": {},
	"testing/iotest": {},
	"testing/quick": {},
	"testing/slogtest": {},
	"testing/synctest": {},
	"text/scanner": {},
	"text/tabwriter": {},
	"text/template": {},
	"text/template/parse": {},
	"time": {},
	"time/tzdata": {},
	"unicode": {},
	"unicode/utf16": {},
	"unicode/utf8": {},
	"unique": {},
	"unsafe": {},
	"uuid": {},
	"weak": {},
}

func isStdlibPackagePath(path string) bool {
	if path == "" {
		return true
	}
	_, ok := stdlibPackagePaths[path]
	return ok
}

func errorPkgPath(err error) string {
	t := reflect.TypeOf(err)
	if t == nil {
		return ""
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.PkgPath()
}

// sameError reports whether a and b are the same error value without
// panicking on non-comparable dynamic types.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if !va.IsValid() || !vb.IsValid() || va.Type() != vb.Type() {
		return false
	}
	if va.Comparable() {
		return va.Interface() == vb.Interface()
	}
	switch va.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer, reflect.Slice:
		return va.Pointer() == vb.Pointer()
	default:
		return false
	}
}

// replaceUnwrappedCause keeps wrapper diagnostics after a nested context
// branch was stripped. Typed net.OpError values are cloned; other wrappers
// keep their Error() text and As/classification surface while suppressing
// context sentinel matches.
func replaceUnwrappedCause(err, filtered error) error {
	// Match only the current node. Nested transport wrappers are rebuilt by
	// recursion so outer wrappers keep their diagnostics.
	switch typed := err.(type) {
	case *net.OpError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *os.PathError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *os.SyscallError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *os.LinkError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *url.Error:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *net.DNSConfigError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *net.DNSError:
		clone := *typed
		clone.UnwrapErr = filtered
		// Err is the cached rendering used by Error(). Refresh it only when it
		// embeds the old UnwrapErr text; otherwise it is an independent DNS
		// reason such as "no such host" and must be preserved.
		if filtered != nil && typed.UnwrapErr != nil {
			if old := typed.UnwrapErr.Error(); old != "" && strings.Contains(clone.Err, old) {
				clone.Err = strings.ReplaceAll(clone.Err, old, filtered.Error())
			}
		}
		return &clone
	case *strconv.NumError:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *exec.Error:
		clone := *typed
		clone.Err = filtered
		return &clone
	case *json.MarshalerError:
		clone := *typed
		clone.Err = filtered
		return &clone
	}
	// Generic fallback for other stdlib carriers with an exported Err error
	// field so As keeps the concrete type with the filtered child.
	if cloned, ok := cloneExportedErrField(err, filtered); ok {
		return cloned
	}
	return &contextStrippedCause{original: err, cause: filtered}
}

// cloneExportedErrField rebuilds a standard-library error whose exported Err
// field holds the wrapped cause, preserving whether the original was a pointer
// or value type (for example template.ExecError vs *os.PathError).
func cloneExportedErrField(err, filtered error) (error, bool) {
	if !isStdlibPackagePath(errorPkgPath(err)) {
		return nil, false
	}
	v := reflect.ValueOf(err)
	wasPointer := false
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, false
		}
		wasPointer = true
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil, false
	}
	errorType := reflect.TypeOf((*error)(nil)).Elem()
	field := v.FieldByName("Err")
	if !field.IsValid() || field.Type() != errorType {
		return nil, false
	}
	clone := reflect.New(v.Type()).Elem()
	clone.Set(v)
	errField := clone.FieldByName("Err")
	if !errField.CanSet() {
		return nil, false
	}
	if filtered == nil {
		errField.Set(reflect.Zero(errorType))
	} else {
		errField.Set(reflect.ValueOf(filtered))
	}
	if wasPointer {
		ptr := reflect.New(v.Type())
		ptr.Elem().Set(clone)
		cloned, ok := ptr.Interface().(error)
		return cloned, ok
	}
	cloned, ok := clone.Interface().(error)
	return cloned, ok
}

// contextStrippedCause retains an original wrapper's diagnostics and As
// surface after nested context branches were removed from the cause chain.
type contextStrippedCause struct {
	original error
	cause    error
}

func (e *contextStrippedCause) Error() string {
	if e == nil || e.original == nil {
		return "<nil>"
	}
	orig := e.original.Error()
	if e.cause == nil {
		// Context-only wrappers still need their discarded child text removed
		// so retained diagnostics do not contradict the filtered Is surface.
		if inners := multiUnwrap(e.original); len(inners) > 0 {
			if cleaned := replaceDiscardedErrorText(orig, inners, nil); cleaned != "" {
				return cleaned
			}
			// Duplicate identical context children make framed reconstruction
			// ambiguous; with nothing retained, treat the rewrite as empty
			// rather than restoring the original context-only text.
			if allDiscardedContextChildren(inners) {
				return ""
			}
		}
		if innerText := unfilteredInnerErrorText(e.original); innerText != "" {
			if cleaned, ok := rewriteInnerErrorText(orig, innerText, ""); ok {
				return cleaned
			}
			// Multiple copies of the child text are ambiguous (diagnostic prose
			// vs the actual unwrap rendering); keep the original safe message.
			if strings.Count(orig, innerText) > 1 {
				return orig
			}
		}
		return stripContextSentinelPhrases(orig)
	}
	causeText := e.cause.Error()
	// Preserve wrapper framing (for example "follow logs: " or "follow %w failed")
	// while replacing the unfiltered inner rendering so stripped context
	// sentinels do not remain in user-facing diagnostics.
	if innerText := unfilteredInnerErrorText(e.original); innerText != "" {
		if cleaned, ok := rewriteInnerErrorText(orig, innerText, causeText); ok {
			return cleaned
		}
		// Repeated copies of the same inner text are ambiguous; fall through
		// so multi-child framing or the filtered cause text is preferred over
		// ReplaceAll corruption of independent diagnostic prose.
	}
	if inners := multiUnwrap(e.original); len(inners) > 0 {
		if cleaned := replaceDiscardedErrorText(orig, inners, e.cause); cleaned != "" {
			return cleaned
		}
		// When Error() omits any child rendering (full or partial sanitization),
		// keep the safe text but still remove discarded context spans that are
		// present, without synthesizing omitted child text.
		if !allChildTextAppearsIn(orig, inners) {
			return stripVerifiedDiscardedChildText(orig, inners)
		}
		// Ambiguous multi-child framing: prefer filtered cause text.
		return causeText
	}
	// Single-error wrapper whose Error() omits the inner text: prefer the
	// sanitized original rendering, except diagnostic-redactor facades whose
	// cause was already re-redacted and must not keep mixed context wording.
	if innerText := unfilteredInnerErrorText(e.original); innerText != "" && !strings.Contains(orig, innerText) {
		type redactorProvider interface {
			DiagnosticRedactor() *cli.Redactor
		}
		if _, ok := e.original.(redactorProvider); ok {
			return causeText
		}
		return orig
	}
	return causeText
}

// rewriteInnerErrorText replaces the sole rendering of the unfiltered inner
// text. When the same phrase appears more than once, the copy may be
// independent diagnostic prose rather than the unwrap rendering (for example
// "peer reported context canceled; context canceled"); callers must treat that
// as ambiguous instead of ReplaceAll-corrupting the retained wording.
// ok is true when the unique occurrence was rewritten, even if the result is
// empty (fully stripped context-only wrappers must not restore orig).
func rewriteInnerErrorText(orig, innerText, repl string) (string, bool) {
	if innerText == "" || !strings.Contains(orig, innerText) {
		return "", false
	}
	if strings.Count(orig, innerText) != 1 {
		return "", false
	}
	cleaned := strings.Replace(orig, innerText, repl, 1)
	if cleaned == orig {
		// Unique child text already matches the filtered rendering; still a
		// successful reconstruction so callers keep wrapper framing instead of
		// falling back to cause-only text.
		if repl == innerText {
			return orig, true
		}
		return "", false
	}
	// Do not TrimSpace: retained diagnostics may intentionally begin or end
	// with whitespace (for example " backend: context canceled "). Only the
	// child phrase itself is removed; adjacent framing stays with the caller.
	return cleaned, true
}

func multiUnwrap(err error) []error {
	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		return unwrapper.Unwrap()
	}
	return nil
}

func allDiscardedContextChildren(inners []error) bool {
	saw := false
	for _, inner := range inners {
		if inner == nil {
			continue
		}
		saw = true
		if nonContextCause(inner) != nil {
			return false
		}
	}
	return saw
}

func allChildTextAppearsIn(orig string, inners []error) bool {
	// Each child needs its own occurrence. A coalesced rendering such as
	// "context canceled; public" must not count as fully rendering two
	// identical "public" children.
	occupied := make([]bool, len(orig))
	claim := func(phrase string) bool {
		for i := 0; i+len(phrase) <= len(orig); i++ {
			if orig[i:i+len(phrase)] != phrase {
				continue
			}
			overlap := false
			for j := i; j < i+len(phrase); j++ {
				if occupied[j] {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			for j := i; j < i+len(phrase); j++ {
				occupied[j] = true
			}
			return true
		}
		return false
	}
	for _, inner := range inners {
		if inner == nil {
			continue
		}
		phrase := inner.Error()
		if phrase == "" {
			continue
		}
		if !claim(phrase) {
			return false
		}
	}
	return true
}

// stripVerifiedDiscardedChildText removes discarded child renderings that are
// present in orig and rewrites retained children to their filtered text,
// without inventing text for omitted (sanitized) children. Retained child
// spans are claimed first so a discarded sentinel is not taken from inside a
// legitimate diagnostic such as "peer reported context canceled".
func stripVerifiedDiscardedChildText(orig string, inners []error) string {
	type edit struct {
		start, end int
		repl       string
	}
	occupied := make([]bool, len(orig))
	claim := func(phrase string) (int, bool) {
		for i := 0; i+len(phrase) <= len(orig); i++ {
			if orig[i:i+len(phrase)] != phrase {
				continue
			}
			overlap := false
			for j := i; j < i+len(phrase); j++ {
				if occupied[j] {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			for j := i; j < i+len(phrase); j++ {
				occupied[j] = true
			}
			return i, true
		}
		return 0, false
	}

	type item struct {
		inner   error
		phrase  string
		removal bool
		order   int
	}
	items := make([]item, 0, len(inners))
	for i, inner := range inners {
		if inner == nil {
			continue
		}
		phrase := inner.Error()
		if phrase == "" {
			continue
		}
		items = append(items, item{
			inner:   inner,
			phrase:  phrase,
			removal: nonContextCause(inner) == nil,
			order:   i,
		})
	}
	// Phrases shared by, or nested inside, retained and discarded children
	// cannot be attributed safely without the format string (for example
	// discarded "context canceled" vs retained "canceled"); keep the original
	// safe text rather than letting the longer discarded claim erase the
	// retained diagnostic.
	retainedPhrase := make(map[string]struct{})
	discardedPhrase := make(map[string]struct{})
	for _, it := range items {
		if it.removal {
			discardedPhrase[it.phrase] = struct{}{}
		} else {
			retainedPhrase[it.phrase] = struct{}{}
		}
	}
	for d := range discardedPhrase {
		for r := range retainedPhrase {
			if d == r || strings.Contains(d, r) || strings.Contains(r, d) {
				return orig
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		if len(items[i].phrase) != len(items[j].phrase) {
			return len(items[i].phrase) > len(items[j].phrase)
		}
		return items[i].order < items[j].order
	})

	// Claim every child span before expanding removals so separators that
	// belong to a retained phrase (for example the leading space of
	// " backend ") stay occupied and are not absorbed into a discarded span.
	type claimed struct {
		inner      error
		start, end int
		removal    bool
	}
	claimedSpans := make([]claimed, 0, len(items))
	for _, it := range items {
		start, ok := claim(it.phrase)
		if !ok {
			// Discarded or omitted (sanitized) child text may be absent;
			// continue so verified discarded spans can still be removed.
			continue
		}
		claimedSpans = append(claimedSpans, claimed{
			inner:   it.inner,
			start:   start,
			end:     start + len(it.phrase),
			removal: it.removal,
		})
	}
	edits := make([]edit, 0, len(claimedSpans))
	changed := false
	for _, c := range claimedSpans {
		if c.removal {
			rs, re := expandRemovalSeparators(orig, c.start, c.end, occupied)
			// Mark the expanded region so a later adjacent discarded child cannot
			// claim the same separator and produce overlapping original offsets.
			for j := rs; j < re; j++ {
				occupied[j] = true
			}
			edits = append(edits, edit{start: rs, end: re, repl: ""})
			changed = true
			continue
		}
		filtered := nonContextCause(c.inner)
		repl := filtered.Error()
		edits = append(edits, edit{start: c.start, end: c.end, repl: repl})
		if repl != orig[c.start:c.end] {
			changed = true
		}
	}
	// Ambiguous framing copies of a discarded phrase, or extra copies of a
	// changed retained phrase, are unsafe to rewrite (one filtered + one stale).
	for _, it := range items {
		phrase := it.phrase
		if !it.removal {
			filtered := nonContextCause(it.inner)
			if filtered == nil || filtered.Error() == phrase {
				continue
			}
		}
		for i := 0; i+len(phrase) <= len(orig); i++ {
			if orig[i:i+len(phrase)] != phrase {
				continue
			}
			free := true
			for j := i; j < i+len(phrase); j++ {
				if occupied[j] {
					free = false
					break
				}
			}
			if free {
				return orig
			}
		}
	}
	if !changed {
		return orig
	}
	// Merge overlapping or abutting removals (empty replacements) so applying
	// original offsets cannot shift a later span into retained text.
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}
		return edits[i].end < edits[j].end
	})
	merged := make([]edit, 0, len(edits))
	for _, e := range edits {
		if e.repl == "" && len(merged) > 0 && merged[len(merged)-1].repl == "" && e.start <= merged[len(merged)-1].end {
			if e.end > merged[len(merged)-1].end {
				merged[len(merged)-1].end = e.end
			}
			continue
		}
		merged = append(merged, e)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].start != merged[j].start {
			return merged[i].start > merged[j].start
		}
		return merged[i].end > merged[j].end
	})
	for i := 1; i < len(merged); i++ {
		if merged[i].end > merged[i-1].start {
			return orig
		}
	}
	cleaned := orig
	for _, e := range merged {
		if e.start < 0 || e.end > len(cleaned) || e.start > e.end {
			return orig
		}
		cleaned = cleaned[:e.start] + e.repl + cleaned[e.end:]
	}
	return cleaned
}

// replaceDiscardedErrorText removes or rewrites child renderings independently
// of Unwrap order so indexed %w verbs that reorder children still strip the
// discarded sentinel occurrence and emit each retained child's filtered text.
// When multiple children share identical Error text, or a shorter discarded
// phrase can only be placed inside a longer retained child's rendering,
// positions cannot be associated safely without the format string; return ""
// so callers fall back to the filtered cause text.
func replaceDiscardedErrorText(orig string, inners []error, _ error) string {
	phraseCount := make(map[string]int)
	for _, inner := range inners {
		if inner == nil {
			continue
		}
		if phrase := inner.Error(); phrase != "" {
			phraseCount[phrase]++
		}
	}
	for _, n := range phraseCount {
		if n > 1 {
			return ""
		}
	}

	type item struct {
		inner  error
		phrase string
		order  int
	}
	items := make([]item, 0, len(inners))
	for i, inner := range inners {
		if inner == nil {
			continue
		}
		phrase := inner.Error()
		if phrase == "" {
			continue
		}
		items = append(items, item{inner: inner, phrase: phrase, order: i})
	}
	// Claim longer phrases first so a discarded sentinel such as
	// "context canceled" is not taken from inside a retained diagnostic like
	// "peer reported context canceled".
	sort.SliceStable(items, func(i, j int) bool {
		if len(items[i].phrase) != len(items[j].phrase) {
			return len(items[i].phrase) > len(items[j].phrase)
		}
		return items[i].order < items[j].order
	})

	type edit struct {
		start, end int
		repl       string
	}
	occupied := make([]bool, len(orig))
	claim := func(phrase string) (int, bool) {
		for i := 0; i+len(phrase) <= len(orig); i++ {
			if orig[i:i+len(phrase)] != phrase {
				continue
			}
			overlap := false
			for j := i; j < i+len(phrase); j++ {
				if occupied[j] {
					overlap = true
					break
				}
			}
			if overlap {
				continue
			}
			for j := i; j < i+len(phrase); j++ {
				occupied[j] = true
			}
			return i, true
		}
		return 0, false
	}

	type pendingEdit struct {
		inner      error
		start, end int
		removal    bool
	}
	pending := make([]pendingEdit, 0, len(items))
	for _, it := range items {
		start, ok := claim(it.phrase)
		if !ok {
			// A child rendering could not be placed without overlapping another
			// claimed span; framed rewrite would mis-associate occurrences.
			return ""
		}
		pending = append(pending, pendingEdit{
			inner:   it.inner,
			start:   start,
			end:     start + len(it.phrase),
			removal: nonContextCause(it.inner) == nil,
		})
	}
	// If a child phrase still occurs outside every claimed span, the message
	// has ambiguous framing copies (for example a literal "context canceled"
	// beside a %w of the same text). Fall back rather than rewriting the
	// wrong occurrence.
	for _, it := range items {
		phrase := it.phrase
		for i := 0; i+len(phrase) <= len(orig); i++ {
			if orig[i:i+len(phrase)] != phrase {
				continue
			}
			free := true
			for j := i; j < i+len(phrase); j++ {
				if occupied[j] {
					free = false
					break
				}
			}
			if free {
				return ""
			}
		}
	}
	// Expand removals only after every child span is claimed so separator
	// absorption cannot walk into a retained cause's trailing spaces.
	var edits []edit
	changed := false
	for _, p := range pending {
		start, end := p.start, p.end
		if p.removal {
			start, end = expandRemovalSeparators(orig, start, end, occupied)
			edits = append(edits, edit{start: start, end: end, repl: ""})
			changed = true
			continue
		}
		repl := nonContextCause(p.inner).Error()
		edits = append(edits, edit{start: start, end: end, repl: repl})
		if repl != orig[start:end] {
			changed = true
		}
	}
	if !changed {
		return ""
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start > edits[j].start
		}
		return edits[i].end > edits[j].end
	})
	for i := 1; i < len(edits); i++ {
		// Overlapping edits are unsafe to apply from original offsets.
		if edits[i].end > edits[i-1].start {
			return ""
		}
	}
	masked := orig
	for _, e := range edits {
		if e.start < 0 || e.end > len(masked) || e.start > e.end {
			return ""
		}
		masked = masked[:e.start] + e.repl + masked[e.end:]
	}
	// Do not TrimSpace or strip :/; here: retained causes may intentionally
	// begin/end with whitespace or punctuation (for example " backend ").
	// Separator cleanup for discarded children happens in
	// expandRemovalSeparators before edits apply.
	return masked
}

// expandRemovalSeparators absorbs unoccupied separators adjacent to a discarded
// child. Rightward expansion always cleans framing before the next claimed
// child (or end). Leftward expansion runs only when the entire region back to
// the previous claimed child is separators, so wrapper labels such as
// "follow logs:" or "cancellation:" are not consumed. Expansion never enters
// spans already claimed by another child.
func expandRemovalSeparators(orig string, start, end int, occupied []bool) (int, int) {
	for end < len(orig) && !occupied[end] && isErrorSeparator(orig[end]) {
		end++
	}
	prevEnd := -1
	for i := start - 1; i >= 0; i-- {
		if occupied[i] {
			prevEnd = i + 1
			break
		}
	}
	if prevEnd >= 0 {
		onlySeparators := true
		for i := prevEnd; i < start; i++ {
			if !isErrorSeparator(orig[i]) {
				onlySeparators = false
				break
			}
		}
		if onlySeparators {
			start = prevEnd
		}
	}
	start, end = expandPairedDelimiters(orig, start, end, occupied)
	// Absorb any separators left beside a removed pair (for example the space
	// after "[canceled] backend").
	for end < len(orig) && !occupied[end] && isErrorSeparator(orig[end]) {
		end++
	}
	if prevEnd >= 0 {
		onlySeparators := true
		for i := prevEnd; i < start; i++ {
			if !isErrorSeparator(orig[i]) {
				onlySeparators = false
				break
			}
		}
		if onlySeparators {
			start = prevEnd
		}
	}
	return start, end
}

// expandPairedDelimiters absorbs a balanced (), [], {}, <>, or quote pair that
// wraps only the discarded span (after separator expansion). Internal
// whitespace between the delimiters and the span is included so "[ canceled ]"
// does not leave "[ ]". Pairs around retained children stay intact because
// those spans remain occupied and block walking out to the closer.
func expandPairedDelimiters(orig string, start, end int, occupied []bool) (int, int) {
	for {
		expanded := false
		for _, pair := range [][2]byte{
			{'(', ')'}, {'[', ']'}, {'{', '}'}, {'<', '>'},
			{'"', '"'}, {'\'', '\''},
		} {
			open := start - 1
			for open >= 0 && !occupied[open] && isErrorSeparator(orig[open]) {
				open--
			}
			close := end
			for close < len(orig) && !occupied[close] && isErrorSeparator(orig[close]) {
				close++
			}
			if open < 0 || close >= len(orig) || occupied[open] || occupied[close] {
				continue
			}
			if orig[open] == pair[0] && orig[close] == pair[1] {
				start, end = open, close+1
				expanded = true
				break
			}
		}
		if !expanded {
			return start, end
		}
	}
}

func isErrorSeparator(b byte) bool {
	// Treat control/whitespace and unpaired framing punctuation as separators
	// so compact multi-wraps (%w - %w, %w/%w, etc.) do not leave leading junk.
	// Paired delimiters such as ()[]{}<> are not absorbed independently; that
	// would leave a dangling closer around a retained child.
	if b <= ' ' {
		return true
	}
	switch b {
	case ';', ':', ',', '|', '-', '/', '\\', '=', '+', '~', '.', '!', '?', '#', '@', '&', '*', '%', '^':
		return true
	default:
		return false
	}
}

// stripContextSentinelPhrases removes canceled/deadline wording from a
// context-only wrapper's Error text when framed reconstruction is unavailable.
// Only a clear classification prefix is stripped; any other occurrence is
// treated as ambiguous and yields a neutral empty message.
func stripContextSentinelPhrases(orig string) string {
	cleaned := orig
	for _, phrase := range []string{"context canceled", "context deadline exceeded"} {
		if strings.HasPrefix(cleaned, phrase) {
			cleaned = strings.TrimPrefix(cleaned, phrase)
			cleaned = strings.TrimSpace(cleaned)
			cleaned = strings.TrimLeft(cleaned, ":;")
			cleaned = strings.TrimSpace(cleaned)
			continue
		}
		if strings.Contains(cleaned, phrase) {
			return ""
		}
	}
	return cleaned
}
func (e *contextStrippedCause) Unwrap() error { return e.cause }

func (e *contextStrippedCause) Is(target error) bool {
	if errors.Is(e.cause, target) {
		return true
	}
	if errors.Is(target, context.Canceled) || errors.Is(target, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(e.original, target)
}

func (e *contextStrippedCause) As(target any) bool {
	if errors.As(e.cause, target) {
		return true
	}
	// Preserve the original wrapper's own As classifications (virtual types
	// not present on the filtered cause). Probe a temporary target first so a
	// custom As that walks unfiltered children cannot leave a discarded
	// context-bearing value in the caller's target.
	if as, ok := e.original.(interface{ As(any) bool }); ok {
		if matchOriginalAsWithoutDiscardedContext(as, target, e.cause) {
			return true
		}
	}
	// Do not expose unfiltered wrappers that still Unwrap to discarded context
	// children. Opaque facades without Unwrap may keep their concrete As type.
	if _, ok := e.original.(interface{ Unwrap() error }); ok {
		return false
	}
	if _, ok := e.original.(interface{ Unwrap() []error }); ok {
		return false
	}
	return asExactType(e.original, target)
}

// matchOriginalAsWithoutDiscardedContext runs original.As into a temporary
// slot and copies the result only when it does not reintroduce a context
// sentinel that the filtered cause suppressed.
func matchOriginalAsWithoutDiscardedContext(as interface{ As(any) bool }, target any, filteredCause error) bool {
	val := reflect.ValueOf(target)
	if val.Kind() != reflect.Pointer || val.IsNil() {
		return false
	}
	tmp := reflect.New(val.Elem().Type())
	if !as.As(tmp.Interface()) {
		return false
	}
	matched := tmp.Elem().Interface()
	if errMatched, ok := matched.(error); ok {
		carriesContext := errors.Is(errMatched, context.Canceled) ||
			errors.Is(errMatched, context.DeadlineExceeded)
		filteredCarriesContext := errors.Is(filteredCause, context.Canceled) ||
			errors.Is(filteredCause, context.DeadlineExceeded)
		if carriesContext && !filteredCarriesContext {
			return false
		}
	}
	val.Elem().Set(tmp.Elem())
	return true
}

func asExactType(err error, target any) bool {
	if err == nil || target == nil {
		return false
	}
	val := reflect.ValueOf(target)
	if val.Kind() != reflect.Pointer || val.IsNil() {
		return false
	}
	dest := val.Elem()
	errVal := reflect.ValueOf(err)
	if !errVal.IsValid() || !errVal.Type().AssignableTo(dest.Type()) {
		return false
	}
	dest.Set(errVal)
	return true
}

func unfilteredInnerErrorText(err error) string {
	if unwrapper, ok := err.(interface{ Unwrap() error }); ok {
		if inner := unwrapper.Unwrap(); inner != nil {
			return inner.Error()
		}
	}
	if unwrapper, ok := err.(interface{ UnwrapRaw() error }); ok {
		if inner := unwrapper.UnwrapRaw(); inner != nil {
			return inner.Error()
		}
	}
	if unwrapper, ok := err.(interface{ Unwrap() []error }); ok {
		if inners := unwrapper.Unwrap(); len(inners) > 0 {
			return errors.Join(inners...).Error()
		}
	}
	return ""
}

func wrapWaitCause(what string, cause error, previous ...error) error {
	if cause == nil {
		return nil
	}
	causes := make([]error, 0, len(previous)+1)
	causes = append(causes, cause)
	causes = append(causes, previous...)
	return newWaitError(fmt.Sprintf("%s: %v", what, cause), causes...)
}

// poll runs check until it succeeds, the container enters a state from
// which startup cannot proceed, or the timeout elapses. Every strategy gets
// the same lifecycle policy: inspect once up front, then at a bounded cadence;
// retain transient check and state errors for final diagnostics.
func poll(ctx context.Context, o options, target Target, what string, check func(context.Context) error) error {
	if err := o.validate(); err != nil {
		return err
	}
	timeout, interval := o.effective()
	callerCtx := ctx
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr, lastProbeErr error
	var lastStateCheck time.Time
	terminationErr := func() error {
		return waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, lastProbeErr))
	}

	if err := terminationErr(); err != nil {
		return err
	}

	state, err := targetState(waitCtx, target)
	if terminalErr := terminationErr(); terminalErr != nil {
		return terminalErr
	}
	if err != nil {
		if permanentProbeError(err) || isPermanentCheckError(err) {
			cause := permanentCause(err)
			if filtered := nonContextCause(cause); filtered != nil {
				return wrapWaitCause(what, filtered, lastProbeErr)
			}
			// Context-only permanent wrapper: continue polling rather than
			// returning an error that still matches context.Canceled.
		} else if filtered := nonContextCause(err); filtered != nil {
			lastProbeErr = filtered
		}
	} else if terminalWaitState(state) {
		return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
	}
	lastStateCheck = time.Now()

	for {
		if err := terminationErr(); err != nil {
			return err
		}

		if err := check(waitCtx); err != nil {
			// Keep the latest non-context check cause even when it races with
			// the context becoming done; the terminal classification must not
			// erase a typed backend error, and mixed Join(context, cause)
			// must not keep the stale context half for later timeouts.
			if filtered := nonContextCause(err); filtered != nil {
				lastErr = filtered
			}
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			if isPermanentCheckError(err) {
				cause := permanentCause(err)
				if filtered := nonContextCause(cause); filtered != nil {
					return wrapWaitCause(what, filtered, joinNonNil(lastErr, lastProbeErr))
				}
				// Context-only permanent wrapper while the wait is still
				// active: treat as transient and keep polling.
			}
		} else {
			if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			}
			causes := joinNonNil(lastErr, lastProbeErr)
			if err := finalLifecycleError(callerCtx, waitCtx, target, what, timeout, interval, causes); err != nil {
				return err
			}
			return terminationErr()
		}

		if err := terminationErr(); err != nil {
			return err
		}
		if time.Since(lastStateCheck) >= stateCheckInterval && waitCtx.Err() == nil {
			lastStateCheck = time.Now()
			state, err := targetState(waitCtx, target)
			if err != nil {
				if terminalErr := terminationErr(); terminalErr != nil {
					if filtered := nonContextCause(err); filtered != nil {
						lastProbeErr = filtered
					}
					return waitContextTerminationError(callerCtx, waitCtx, what, timeout, joinNonNil(lastErr, lastProbeErr))
				}
				if isPermanentCheckError(err) || permanentProbeError(err) {
					cause := permanentCause(err)
					if filtered := nonContextCause(cause); filtered != nil {
						return wrapWaitCause(what, filtered, joinNonNil(lastErr, lastProbeErr))
					}
					// Context-only permanent wrapper: keep polling.
				} else if filtered := nonContextCause(err); filtered != nil {
					lastProbeErr = filtered
				}
			} else if terminalErr := terminationErr(); terminalErr != nil {
				return terminalErr
			} else if terminalWaitState(state) {
				return waitStoppedStateError(what, state, joinNonNil(lastErr, lastProbeErr))
			}
		}

		timer := time.NewTimer(interval)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			// No post-deadline probe is started. The caller and startup
			// deadlines are the hard budget for every target call.
			return terminationErrOrTimeout(terminationErr(), what, timeout, joinNonNil(lastErr, lastProbeErr))
		case <-timer.C:
		}
	}
}

func terminationErrOrTimeout(err error, what string, timeout time.Duration, lastErr error) error {
	if err != nil {
		return err
	}
	return waitTimeoutError(what, timeout, lastErr)
}

func permanentCause(err error) error {
	var fatal fatalCheckError
	if errors.As(err, &fatal) && fatal.err != nil {
		return fatal.err
	}
	var fatalPointer *fatalCheckError
	if errors.As(err, &fatalPointer) && fatalPointer != nil && fatalPointer.err != nil {
		return fatalPointer.err
	}
	return err
}

// fatalCheckError wraps a check error that must end the poll
// immediately instead of being retried: the check cannot succeed as
// configured (for example, a CLI launch failure, unknown container, or
// invalid request), so retrying cannot help.
type fatalCheckError struct{ err error }

func (e fatalCheckError) Error() string {
	if e.err == nil {
		return "permanent wait error"
	}
	return e.err.Error()
}
func (e fatalCheckError) Unwrap() error { return e.err }

func permanentProbeError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTargetNotFound) {
		return true
	}
	return cli.PermanentStartError(err)
}
