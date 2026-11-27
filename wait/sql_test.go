package wait

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// The fake drivers keep ForSQL tests backend-independent and use only the
// standard library, matching the production dependency on database/sql alone.
const (
	fakeSQLDriverName           = "wait-test-sql"
	fakeConnectorDriverName     = "wait-test-sql-connector"
	fakeConnectorRejectedPrefix = "bad-dsn"
)

var fakeSQLStatePtr atomic.Pointer[fakeSQLState]

func init() {
	sql.Register(fakeSQLDriverName, fakeSQLDriver{})
	sql.Register(fakeConnectorDriverName, fakeConnectorDriver{})
}

type fakeSQLDriver struct{}

func (fakeSQLDriver) Open(name string) (driver.Conn, error) {
	state := fakeSQLStatePtr.Load()
	if state == nil {
		return nil, errors.New("fake SQL driver is not configured")
	}
	return state.open(name)
}

// fakeConnectorDriver implements driver.DriverContext so sql.Open parses the
// DSN through OpenConnector, like pgx stdlib does.
type fakeConnectorDriver struct{}

func (fakeConnectorDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("connector driver requires DriverContext")
}

func (fakeConnectorDriver) OpenConnector(dsn string) (driver.Connector, error) {
	if strings.HasPrefix(dsn, fakeConnectorRejectedPrefix) {
		return nil, fmt.Errorf("cannot parse DSN %q", dsn)
	}
	return fakeSQLConnector{}, nil
}

type fakeSQLConnector struct{}

func (fakeSQLConnector) Connect(context.Context) (driver.Conn, error) {
	state := fakeSQLStatePtr.Load()
	if state == nil {
		return nil, errors.New("fake SQL driver is not configured")
	}
	return fakeSQLConn{state: state}, nil
}

func (fakeSQLConnector) Driver() driver.Driver { return fakeConnectorDriver{} }

type fakeSQLState struct {
	mu         sync.Mutex
	opens      int
	pings      int
	queries    int
	lastDSN    string
	lastQuery  string
	columns    []string
	rows       [][]driver.Value
	pingFn     func(ctx context.Context, attempt int) error
	queryFn    func(ctx context.Context, attempt int) error
	rowNexts   int
	rowsNextFn func(ctx context.Context) error
}

func (s *fakeSQLState) open(dsn string) (driver.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opens++
	s.lastDSN = dsn
	return fakeSQLConn{state: s}, nil
}

func (s *fakeSQLState) countPing(ctx context.Context) error {
	s.mu.Lock()
	s.pings++
	attempt := s.pings
	fn := s.pingFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, attempt)
	}
	return nil
}

func (s *fakeSQLState) countQuery(ctx context.Context, query string) error {
	s.mu.Lock()
	s.queries++
	attempt := s.queries
	s.lastQuery = query
	fn := s.queryFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx, attempt)
	}
	return nil
}

func (s *fakeSQLState) dsn() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastDSN
}

func (s *fakeSQLState) query() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastQuery
}

func (s *fakeSQLState) setPingFunc(fn func(ctx context.Context, attempt int) error) {
	s.mu.Lock()
	s.pingFn = fn
	s.mu.Unlock()
}

func (s *fakeSQLState) setQueryFunc(fn func(ctx context.Context, attempt int) error) {
	s.mu.Lock()
	s.queryFn = fn
	s.mu.Unlock()
}

func (s *fakeSQLState) setRows(columns []string, rows [][]driver.Value) {
	s.mu.Lock()
	s.columns, s.rows = columns, rows
	s.mu.Unlock()
}

func (s *fakeSQLState) resultRows() *fakeSQLRows {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &fakeSQLRows{columns: s.columns, rows: s.rows, state: s}
}

func (s *fakeSQLState) countRowNext(ctx context.Context) error {
	s.mu.Lock()
	s.rowNexts++
	fn := s.rowsNextFn
	s.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return io.EOF
}

func (s *fakeSQLState) rowNextCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rowNexts
}

func (s *fakeSQLState) probeCounts() (opens, pings, queries int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens, s.pings, s.queries
}

type fakeSQLConn struct{ state *fakeSQLState }

func (fakeSQLConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare is not supported")
}

func (fakeSQLConn) Close() error { return nil }

func (fakeSQLConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions are not supported")
}

func (c fakeSQLConn) Ping(ctx context.Context) error { return c.state.countPing(ctx) }

func (c fakeSQLConn) QueryContext(ctx context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := c.state.countQuery(ctx, query); err != nil {
		return nil, err
	}
	return c.state.resultRows(), nil
}

// fakeSQLRows serves one result set per query. A nil rows list yields no rows,
// which is enough for tests that only need the query to succeed.
type fakeSQLRows struct {
	columns []string
	rows    [][]driver.Value
	index   int
	state   *fakeSQLState
}

func (r *fakeSQLRows) Columns() []string {
	if len(r.columns) == 0 {
		return []string{"ready"}
	}
	return r.columns
}

func (r *fakeSQLRows) Close() error { return nil }

func (r *fakeSQLRows) Next(dest []driver.Value) error {
	if r.state != nil {
		// A test that injects a deferred row error drives Next directly.
		if err := r.state.countRowNext(context.Background()); err != nil && err != io.EOF {
			return err
		}
	}
	if r.index >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.index])
	r.index++
	return nil
}

func withFakeSQLState(t *testing.T, state *fakeSQLState) *fakeSQLState {
	t.Helper()
	fakeSQLStatePtr.Store(state)
	t.Cleanup(func() { fakeSQLStatePtr.Store(nil) })
	return state
}

func sqlTarget(endpoint string) *fakeTarget {
	target := newFakeTarget()
	target.endpoint = endpoint
	return target
}

func sqlDSN(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

func TestForSQLSucceedsAfterRetries(t *testing.T) {
	state := withFakeSQLState(t, &fakeSQLState{pingFn: func(_ context.Context, attempt int) error {
		if attempt < 3 {
			return errors.New("connection refused")
		}
		return nil
	}})
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithStartupTimeout(2*time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	opens, pings, queries := state.probeCounts()
	if pings < 3 || opens < 3 {
		t.Errorf("opens = %d, pings = %d, want repeated attempts", opens, pings)
	}
	if queries != 0 {
		t.Errorf("queries = %d, want the ping probe by default", queries)
	}
}

func TestForSQLTimeoutRetainsProbeError(t *testing.T) {
	cause := errors.New("database is starting up")
	state := withFakeSQLState(t, &fakeSQLState{pingFn: func(context.Context, int) error { return cause }})
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("error = %v, want the probe cause", err)
	}
	_, pings, _ := state.probeCounts()
	if pings < 2 {
		t.Errorf("pings = %d, want repeated polling", pings)
	}
}

func TestForSQLAttemptTimeoutRetriesBlockedPing(t *testing.T) {
	restore := sqlAttemptTimeout
	sqlAttemptTimeout = 20 * time.Millisecond
	t.Cleanup(func() { sqlAttemptTimeout = restore })

	state := withFakeSQLState(t, &fakeSQLState{pingFn: func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}})
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithStartupTimeout(150*time.Millisecond).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want the startup deadline", err)
	}
	_, pings, _ := state.probeCounts()
	if pings < 2 {
		t.Errorf("pings = %d, want a retry after the per-attempt timeout", pings)
	}
}

func TestForSQLWithQuery(t *testing.T) {
	state := withFakeSQLState(t, &fakeSQLState{})
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT 1").
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	_, pings, queries := state.probeCounts()
	if queries == 0 {
		t.Error("query probe was never executed")
	}
	if pings != 0 {
		t.Errorf("pings = %d, want the query probe instead of ping", pings)
	}
	if got := state.query(); got != "SELECT 1" {
		t.Errorf("query = %q, want SELECT 1", got)
	}
}

func TestForSQLQueryRetriesRejectedQuery(t *testing.T) {
	state := withFakeSQLState(t, &fakeSQLState{queryFn: func(_ context.Context, attempt int) error {
		if attempt < 2 {
			return errors.New("relation is not ready")
		}
		return nil
	}})
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT 1").
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	_, _, queries := state.probeCounts()
	if queries < 2 {
		t.Errorf("queries = %d, want a retry after the rejected query", queries)
	}
}

func TestForSQLQueryMatcherValidatesRowsAndRetries(t *testing.T) {
	state := withFakeSQLState(t, &fakeSQLState{})
	state.setRows([]string{"count"}, [][]driver.Value{{int64(2)}})
	var calls atomic.Int32
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT count(*) FROM migrations").
		WithQueryMatcher(func(rows *sql.Rows) (bool, error) {
			if calls.Add(1) == 1 {
				return false, nil
			}
			if !rows.Next() {
				return false, rows.Err()
			}
			var count int
			if err := rows.Scan(&count); err != nil {
				return false, err
			}
			return count > 0, nil
		}).
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if calls.Load() < 2 {
		t.Errorf("matcher calls = %d, want a retry after the rejected result", calls.Load())
	}
	_, _, queries := state.probeCounts()
	if queries < 2 {
		t.Errorf("queries = %d, want the matcher to trigger another attempt", queries)
	}
}

func TestForSQLQueryMatcherErrorIsRetryable(t *testing.T) {
	withFakeSQLState(t, &fakeSQLState{})
	cause := errors.New("result is not populated yet")
	var calls atomic.Int32
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT count(*) FROM migrations").
		WithQueryMatcher(func(*sql.Rows) (bool, error) {
			if calls.Add(1) < 3 {
				return false, cause
			}
			return true, nil
		}).
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if calls.Load() < 3 {
		t.Errorf("matcher calls = %d, want retries after matcher errors", calls.Load())
	}
}

func TestForSQLQueryMatcherPermanentErrorFailsFast(t *testing.T) {
	withFakeSQLState(t, &fakeSQLState{})
	start := time.Now()
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT 1").
		WithQueryMatcher(func(*sql.Rows) (bool, error) {
			return false, fmt.Errorf("matcher is misconfigured: %w", ErrInvalidConfiguration)
		}).
		WithStartupTimeout(5*time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want a fail-fast permanent matcher error", elapsed)
	}
}

func TestForSQLQueryMatcherRequiresQuery(t *testing.T) {
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQueryMatcher(func(*sql.Rows) (bool, error) { return true, nil }).
		WaitUntilReady(context.Background(), newFakeTarget())
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %T %v, want *ConfigError", err, err)
	}
	if configErr.Strategy != "ForSQL" || configErr.Field != "query matcher" {
		t.Errorf("ConfigError = %+v, want strategy ForSQL and field query matcher", configErr)
	}
}

func TestForSQLWithAttemptTimeoutRetriesHungProbe(t *testing.T) {
	blockedPing := func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}
	blockedQuery := func(ctx context.Context, _ int) error {
		<-ctx.Done()
		return ctx.Err()
	}
	for _, tc := range []struct {
		name  string
		build func(*fakeSQLState) *SQLStrategy
	}{
		{
			name: "ping",
			build: func(state *fakeSQLState) *SQLStrategy {
				state.setPingFunc(blockedPing)
				return ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN)
			},
		},
		{
			name: "query",
			build: func(state *fakeSQLState) *SQLStrategy {
				state.setQueryFunc(blockedQuery)
				return ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).WithQuery("SELECT 1")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := withFakeSQLState(t, &fakeSQLState{})
			err := tc.build(state).
				WithAttemptTimeout(20*time.Millisecond).
				WithStartupTimeout(150*time.Millisecond).
				WithPollInterval(2*time.Millisecond).
				WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("error = %v, want the startup deadline", err)
			}
			_, pings, queries := state.probeCounts()
			if pings+queries < 2 {
				t.Errorf("attempts = %d, want a retry after the per-attempt timeout", pings+queries)
			}
		})
	}
}

func TestForSQLAttemptTimeoutRejectsNonPositive(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Millisecond} {
		t.Run(d.String(), func(t *testing.T) {
			err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
				WithAttemptTimeout(d).
				WaitUntilReady(context.Background(), newFakeTarget())
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("error = %T %v, want *ConfigError", err, err)
			}
			if configErr.Strategy != "ForSQL" || configErr.Field != "attempt timeout" {
				t.Errorf("ConfigError = %+v, want strategy ForSQL and field attempt timeout", configErr)
			}
		})
	}
}

func TestForSQLDiagnosticValuesOmitRequestMetadata(t *testing.T) {
	strategy := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT count(*) FROM migrations")
	if values := strategy.DiagnosticValues(); len(values) != 0 {
		t.Fatalf("DiagnosticValues() = %v, want no static request metadata", values)
	}
}

func TestForSQLDSNReceivesResolvedHostAndPort(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint string
		wantHost string
		wantPort int
	}{
		{name: "IPv4", endpoint: "127.0.0.1:5432", wantHost: "127.0.0.1", wantPort: 5432},
		{name: "IPv6", endpoint: "[::1]:6543", wantHost: "::1", wantPort: 6543},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeSQLState(t, &fakeSQLState{})
			var host string
			var port int
			err := ForSQL("5432/tcp", fakeSQLDriverName, func(resolvedHost string, resolvedPort int) string {
				host, port = resolvedHost, resolvedPort
				return sqlDSN(resolvedHost, resolvedPort)
			}).WithStartupTimeout(time.Second).
				WithPollInterval(2*time.Millisecond).
				WaitUntilReady(context.Background(), sqlTarget(tc.endpoint))
			if err != nil {
				t.Fatalf("WaitUntilReady: %v", err)
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("dsn factory got %s:%d, want %s:%d", host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestForSQLUnknownDriverRejectedBeforeProbing(t *testing.T) {
	withFakeSQLState(t, &fakeSQLState{})
	dsn := func(string, int) string { return "" }

	if err := Validate(ForSQL("5432/tcp", "wait-test-missing-driver", dsn)); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("Validate error = %v, want ErrInvalidConfiguration", err)
	}

	start := time.Now()
	err := ForSQL("5432/tcp", "wait-test-missing-driver", dsn).
		WithStartupTimeout(5*time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Errorf("error = %v, want ErrInvalidConfiguration", err)
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) || configErr.Strategy != "ForSQL" || configErr.Field != "driver" {
		t.Errorf("error = %v, want the driver configuration error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate failure", elapsed)
	}
}

func TestForSQLInvalidEndpointFailsFast(t *testing.T) {
	withFakeSQLState(t, &fakeSQLState{})
	for _, endpoint := range []string{"127.0.0.1", "127.0.0.1:not-a-port", "127.0.0.1:70000"} {
		t.Run(endpoint, func(t *testing.T) {
			err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
				WithStartupTimeout(5*time.Second).
				WaitUntilReady(context.Background(), sqlTarget(endpoint))
			if err == nil || !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v, want ErrInvalidConfiguration", err)
			}
		})
	}
}

func TestForSQLDSNRejectedByDriverIsConfigError(t *testing.T) {
	const secret = fakeConnectorRejectedPrefix + "-s3cr3tvalue"
	withFakeSQLState(t, &fakeSQLState{})
	start := time.Now()
	err := ForSQL("5432/tcp", fakeConnectorDriverName, func(string, int) string { return secret }).
		WithStartupTimeout(5*time.Second).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	var configErr *ConfigError
	if !errors.As(err, &configErr) || configErr.Strategy != "ForSQL" || configErr.Field != "dsn" {
		t.Fatalf("error = %v, want the DSN configuration error", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error = %q, must not expose the DSN", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait took %v, want an immediate failure", elapsed)
	}
}

func TestForSQLRedactsDSNInProbeErrors(t *testing.T) {
	const secret = "s3cr3tvalue"
	state := withFakeSQLState(t, &fakeSQLState{})
	state.setPingFunc(func(context.Context, int) error {
		return fmt.Errorf("connect failed using %s", state.dsn())
	})
	err := ForSQL("5432/tcp", fakeSQLDriverName, func(string, int) string { return secret }).
		WithStartupTimeout(40*time.Millisecond).
		WithPollInterval(5*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error = %q, must not expose the DSN", err)
	}
}

func TestForSQLRedactsDerivedDSNValues(t *testing.T) {
	const password = "s3cr3tvalue"
	tests := []struct {
		name string
		dsn  string
	}{
		{name: "URL userinfo", dsn: "postgres://user:" + password + "@127.0.0.1:5432/db?sslmode=disable"},
		{name: "URL query", dsn: "postgres://127.0.0.1:5432/db?password=" + password},
		{name: "key value", dsn: "host=127.0.0.1 port=5432 user=postgres password=" + password + " sslmode=disable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := withFakeSQLState(t, &fakeSQLState{})
			state.setPingFunc(func(context.Context, int) error {
				return fmt.Errorf("authentication failed for %s", password)
			})
			err := ForSQL("5432/tcp", fakeSQLDriverName, func(string, int) string { return tc.dsn }).
				WithStartupTimeout(40*time.Millisecond).
				WithPollInterval(5*time.Millisecond).
				WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
			if err == nil {
				t.Fatal("wait unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), password) {
				t.Fatalf("error = %q, must not expose the DSN password", err)
			}
		})
	}
}

// TestSQLDSNDiagnosticValuesCoverDriverDialects pins the DSN forms drivers
// accept: MySQL's scheme-less userinfo DSN, semicolon-separated ADO/ODBC
// pairs, and libpq key=value fields whose quoted value may contain spaces.
func TestSQLDSNDiagnosticValuesCoverDriverDialects(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		want    []string
		notWant []string
	}{
		{
			name: "MySQL scheme-less userinfo",
			dsn:  "user:mysqlpass@tcp(127.0.0.1:3306)/db",
			want: []string{"user:mysqlpass", "mysqlpass"},
		},
		{
			name: "MySQL userinfo and query parameter",
			dsn:  "user:mysqlpass@tcp(127.0.0.1:3306)/db?timeout=1s&Password=querypass",
			want: []string{"mysqlpass", "querypass"},
		},
		{
			name: "semicolon separated ODBC fields",
			dsn:  "Server=127.0.0.1;User Id=sa;Password=odbcpass;",
			want: []string{"odbcpass"},
		},
		{
			name: "semicolon separated braced value",
			dsn:  "Server=127.0.0.1;Password={p;a}}b};Database=db",
			want: []string{"p;a}b"},
		},
		{
			name:    "libpq single-quoted value with spaces",
			dsn:     "host=127.0.0.1 user=postgres password='my secretvalue' dbname=db",
			want:    []string{"my secretvalue"},
			notWant: []string{"my", "secretvalue"},
		},
		{
			name: "libpq escaped quote",
			dsn:  `host=127.0.0.1 user=postgres password='it\'s secret'`,
			want: []string{"it's secret"},
		},
		{
			name: "URL query keys are case-insensitive and use secret names",
			dsn:  "postgres://user:urlpass@127.0.0.1:5432/db?Password=querypass&token=tok&sslpassword=sslpass&sslmode=disable",
			want: []string{"urlpass", "querypass", "tok", "sslpass"},
		},
		{
			name: "escaped URL userinfo",
			dsn:  "postgres://user:p%40ss@127.0.0.1:5432/db",
			want: []string{"p%40ss", "p@ss"},
		},
		{
			name: "malformed URL still yields its query secret",
			dsn:  "postgres://%zz?password=badpass",
			want: []string{"badpass"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values := sqlDSNDiagnosticValues(tc.dsn)
			for _, want := range tc.want {
				if !slices.Contains(values, want) {
					t.Errorf("sqlDSNDiagnosticValues(%q) = %v, missing %q", tc.dsn, values, want)
				}
			}
			for _, unwanted := range tc.notWant {
				if slices.Contains(values, unwanted) {
					t.Errorf("sqlDSNDiagnosticValues(%q) = %v, contains %q", tc.dsn, values, unwanted)
				}
			}
		})
	}
}

func TestForSQLRedactsQuotedDSNPasswordWithSpace(t *testing.T) {
	const password = "my secretvalue"
	for _, tc := range []struct {
		name string
		dsn  string
	}{
		{name: "libpq quoted", dsn: "host=127.0.0.1 user=postgres password='" + password + "' dbname=db"},
		{name: "ODBC braced", dsn: "Server=127.0.0.1;User Id=sa;Password={" + password + "};"},
		{name: "MySQL userinfo", dsn: "user:" + password + "@tcp(127.0.0.1:3306)/db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := withFakeSQLState(t, &fakeSQLState{})
			state.setPingFunc(func(context.Context, int) error {
				return fmt.Errorf("authentication failed for %q", password)
			})
			err := ForSQL("5432/tcp", fakeSQLDriverName, func(string, int) string { return tc.dsn }).
				WithStartupTimeout(40*time.Millisecond).
				WithPollInterval(5*time.Millisecond).
				WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
			if err == nil {
				t.Fatal("wait unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), password) || strings.Contains(err.Error(), "secretvalue") {
				t.Fatalf("error = %q, must not expose the DSN password", err)
			}
		})
	}
}

func TestForAllTimeoutKeepsSQLDSNRedaction(t *testing.T) {
	const password = "composite-dsn-password"
	state := withFakeSQLState(t, &fakeSQLState{})
	state.setPingFunc(func(context.Context, int) error {
		return &cli.CLIError{
			Binary:   "psql",
			Args:     []string{"-c", "select 1"},
			ExitCode: 1,
			Stderr:   "authentication failed for " + password,
		}
	})
	dsn := "host=127.0.0.1 user=postgres password=" + password
	strategy := ForAll(
		ForSQL("5432/tcp", fakeSQLDriverName, func(string, int) string { return dsn }).
			WithStartupTimeout(5 * time.Second).
			WithPollInterval(5 * time.Millisecond),
	).WithStartupTimeout(60 * time.Millisecond)
	err := strategy.WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil {
		t.Fatal("wait unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("error = %q, must retain the SQL DSN redactor across the composite timeout", err)
	}
	var cliErr *cli.CLIError
	if errors.As(err, &cliErr) && strings.Contains(cliErr.Stderr, password) {
		t.Fatalf("errors.As CLIError = %q, must not expose the DSN password", cliErr.Error())
	}
}

func TestForSQLEmptyPortUsesImplicitSelection(t *testing.T) {
	withFakeSQLState(t, &fakeSQLState{})
	target := &portRecordingSQLTarget{fakeTarget: newFakeTarget()}
	target.endpoint = "127.0.0.1:5432"
	err := ForSQL("", fakeSQLDriverName, sqlDSN).
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), target)
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if target.port != "" {
		t.Errorf("Endpoint port = %q, want the implicit empty selection", target.port)
	}
}

type portRecordingSQLTarget struct {
	*fakeTarget
	port string
}

func (t *portRecordingSQLTarget) Endpoint(ctx context.Context, port string) (string, error) {
	t.port = port
	return t.fakeTarget.Endpoint(ctx, port)
}

func TestForSQLPortValidation(t *testing.T) {
	dsn := func(string, int) string { return "" }
	if err := ValidateWithPorts(ForSQL("5432/tcp", fakeSQLDriverName, dsn), []string{"5432/tcp"}); err != nil {
		t.Errorf("ValidateWithPorts declared port: %v", err)
	}
	if err := ValidateWithPorts(ForSQL("6379/tcp", fakeSQLDriverName, dsn), []string{"5432/tcp"}); !errors.Is(err, ErrPortNotExposed) {
		t.Errorf("ValidateWithPorts undeclared port: %v, want ErrPortNotExposed", err)
	}
	if err := ValidateWithPorts(ForSQL("", fakeSQLDriverName, dsn), []string{"5432/tcp"}); err != nil {
		t.Errorf("ValidateWithPorts implicit TCP port: %v", err)
	}
	if err := ValidateWithPorts(ForSQL("", fakeSQLDriverName, dsn), []string{"5432/udp"}); !errors.Is(err, ErrPortNotExposed) {
		t.Errorf("ValidateWithPorts implicit UDP-only target: %v, want ErrPortNotExposed", err)
	}
}

func TestForSQLConfigErrors(t *testing.T) {
	dsn := func(string, int) string { return "" }
	tests := []struct {
		name  string
		build func() *SQLStrategy
		field string
	}{
		{name: "empty driver", build: func() *SQLStrategy { return ForSQL("5432/tcp", "", dsn) }, field: "driver"},
		{name: "unregistered driver", build: func() *SQLStrategy {
			return ForSQL("5432/tcp", "wait-test-missing-driver", dsn)
		}, field: "driver"},
		{name: "nil dsn", build: func() *SQLStrategy { return ForSQL("5432/tcp", fakeSQLDriverName, nil) }, field: "dsn factory"},
		{name: "invalid port", build: func() *SQLStrategy { return ForSQL("not-a-port", fakeSQLDriverName, dsn) }, field: "port specification"},
		{name: "empty query", build: func() *SQLStrategy { return ForSQL("5432/tcp", fakeSQLDriverName, dsn).WithQuery("  ") }, field: "query"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.build().WaitUntilReady(context.Background(), newFakeTarget())
			if err == nil {
				t.Fatal("wait unexpectedly succeeded")
			}
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("error = %T %v, want *ConfigError", err, err)
			}
			if configErr.Strategy != "ForSQL" || configErr.Field != tc.field {
				t.Errorf("ConfigError = %+v, want strategy ForSQL and field %q", configErr, tc.field)
			}
		})
	}
}

func TestForSQLDiagnosticValuesOmitDSN(t *testing.T) {
	const secret = "s3cr3tvalue"
	withFakeSQLState(t, &fakeSQLState{})
	strategy := ForSQL("5432/tcp", fakeSQLDriverName, func(string, int) string { return secret })
	assertNoDSN := func() {
		t.Helper()
		for _, value := range strategy.DiagnosticValues() {
			if strings.Contains(value, secret) {
				t.Fatalf("DiagnosticValues = %v, must not include the wait's DSN", strategy.DiagnosticValues())
			}
		}
	}
	assertNoDSN()
	if err := strategy.WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432")); err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	assertNoDSN()
}

func TestForSQLWithQueryIteratesRows(t *testing.T) {
	state := withFakeSQLState(t, &fakeSQLState{})
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT 1").
		WithStartupTimeout(time.Second).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err != nil {
		t.Fatalf("WaitUntilReady: %v", err)
	}
	if got := state.rowNextCount(); got == 0 {
		t.Fatal("query probe never called rows.Next")
	}
}

func TestForSQLWithQueryRetriesDeferredRowError(t *testing.T) {
	state := withFakeSQLState(t, &fakeSQLState{
		rowsNextFn: func(context.Context) error { return errors.New("relation is not ready") },
	})
	err := ForSQL("5432/tcp", fakeSQLDriverName, sqlDSN).
		WithQuery("SELECT 1").
		WithStartupTimeout(60*time.Millisecond).
		WithPollInterval(2*time.Millisecond).
		WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want the startup deadline after retrying the row error", err)
	}
	if _, _, queries := state.probeCounts(); queries < 2 {
		t.Errorf("queries = %d, want a retry after the deferred row error", queries)
	}
	if got := state.rowNextCount(); got < 2 {
		t.Errorf("rows.Next calls = %d, want repeated probes to iterate rows", got)
	}
}

func TestForSQLDiagnosticValuesKeepConnectionMetadata(t *testing.T) {
	const driver = "wait-test-diagnostic-driver"
	strategy := ForSQL("5432/tcp", driver, func(string, int) string { return "" })
	for _, value := range strategy.DiagnosticValues() {
		if value == "5432/tcp" || value == driver {
			t.Fatalf("DiagnosticValues = %v, must not register non-secret connection metadata", strategy.DiagnosticValues())
		}
	}

	err := strategy.WithStartupTimeout(time.Second).WaitUntilReady(context.Background(), sqlTarget("127.0.0.1:5432"))
	if err == nil || !strings.Contains(err.Error(), driver) {
		t.Fatalf("error = %v, want the unregistered driver name in the diagnostic", err)
	}
	if strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("error = %q, must not redact connection metadata", err)
	}
}
