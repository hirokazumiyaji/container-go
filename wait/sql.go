package wait

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sqlAttemptTimeout bounds one database/sql probe attempt so a stalled
// connection handshake cannot consume the whole startup budget. Overridable in
// tests.
var sqlAttemptTimeout = 3 * time.Second

// sqlOpen is sql.Open, overridable in tests that count database handles.
var sqlOpen = sql.Open

// SQLStrategy waits until a database/sql probe succeeds against a container's
// declared port. It holds no per-wait state, so one value may be reused for
// concurrent waits.
type SQLStrategy struct {
	options
	port     string
	driver   string
	dsn      func(host string, port int) string
	query    string
	querySet bool
}

// ForSQL waits until a SQL connection to port can execute a readiness probe.
//
// The package deliberately imports no database driver. Register one in the
// caller (for example github.com/lib/pq as "postgres" or
// github.com/jackc/pgx/v5/stdlib as "pgx") and pass its driver name; an
// unregistered driver is rejected by Validate, so WithWaitStrategy reports it
// before any container work, and again at the start of the wait. dsn is called
// for every attempt with the host and port resolved from the target, so
// credentials and connection options stay caller-owned. The default probe is
// db.PingContext; WithQuery replaces it with a query that must succeed.
//
// host is returned without IPv6 brackets (net.SplitHostPort removes them).
// Build URL-style DSNs with net.JoinHostPort(host, strconv.Itoa(port)) instead
// of formatting "host:port" directly.
//
// Waiting on SQL rather than only on the listening port covers databases that
// accept connections but reject them while initialization is still running
// (for example the restart after an initdb), and a readiness statement also
// proves the database is accepting queries.
//
// An empty port asks the target for its first declared TCP port, like
// ForExposedPort. The default startup timeout is 60s and the default poll
// interval is 100ms. Each attempt runs under a 3s deadline; an expired attempt
// is retried rather than reported as a configuration error. An
// undeclared port, an empty or unregistered driver, a nil dsn function, an
// invalid port specification, and an endpoint that is not host:port are
// configuration errors that fail immediately.
//
// The DSN is registered for redaction of the wait error produced here,
// together with the password-derived forms recoverable with the standard
// library (URL userinfo/password, URL query password parameters, and key=value
// password fields). A log tail attached by a higher-level helper after the
// wait fails is outside that redaction because the DSN is not known before the
// wait starts.
func ForSQL(port, driver string, dsn func(host string, port int) string) *SQLStrategy {
	return &SQLStrategy{port: port, driver: driver, dsn: dsn}
}

// clone returns a copy of the strategy. Builders derive a new value from the
// receiver so a strategy shared by parallel callers cannot be reconfigured
// through another caller's options.
func (s *SQLStrategy) clone() *SQLStrategy {
	cloned := *s
	return &cloned
}

// WithQuery runs query instead of the default connection ping. The query must
// be one that returns rows, such as "SELECT 1"; its rows are closed once the
// driver accepts it. An empty or whitespace-only query is rejected as a
// *ConfigError.
func (s *SQLStrategy) WithQuery(query string) *SQLStrategy {
	cloned := s.clone()
	cloned.query, cloned.querySet = query, true
	return cloned
}

func (s *SQLStrategy) WithStartupTimeout(d time.Duration) *SQLStrategy {
	cloned := s.clone()
	cloned.startupTimeout = d
	return cloned
}

func (s *SQLStrategy) WithPollInterval(d time.Duration) *SQLStrategy {
	cloned := s.clone()
	cloned.pollInterval = d
	return cloned
}

// DiagnosticValues returns only the strategy's static values. A DSN is built
// during a wait and therefore stays with that wait's error.
func (s *SQLStrategy) DiagnosticValues() []string {
	return []string{s.port, s.driver, s.query}
}

func (s *SQLStrategy) validate() error {
	if err := s.options.validate("ForSQL"); err != nil {
		return err
	}
	if s.port != "" {
		if err := validateTCPPortSpec("ForSQL", s.port); err != nil {
			return err
		}
	}
	if s.driver == "" {
		return &ConfigError{
			Strategy: "ForSQL",
			Field:    "driver",
			Reason:   "driver must not be empty",
		}
	}
	if !slices.Contains(sql.Drivers(), s.driver) {
		return &ConfigError{
			Strategy: "ForSQL",
			Field:    "driver",
			Value:    s.driver,
			Reason:   "driver is not registered; import and register it before waiting",
		}
	}
	if s.dsn == nil {
		return &ConfigError{
			Strategy: "ForSQL",
			Field:    "dsn factory",
			Reason:   "dsn factory must not be nil",
		}
	}
	if s.querySet && strings.TrimSpace(s.query) == "" {
		return &ConfigError{
			Strategy: "ForSQL",
			Field:    "query",
			Reason:   "query must not be empty",
		}
	}
	return nil
}

func (s *SQLStrategy) WaitUntilReady(ctx context.Context, target Target) (retErr error) {
	// seen accumulates the DSNs of this wait only. Keeping them local leaves
	// the strategy reusable and race-free while still redacting every value
	// that can appear in this wait's error.
	var seen []string
	seenSet := make(map[string]struct{})
	addSeen := func(values []string) {
		for _, value := range values {
			if _, ok := seenSet[value]; ok {
				continue
			}
			seenSet[value] = struct{}{}
			seen = append(seen, value)
		}
	}
	defer func() {
		retErr = safeDiagnosticError(retErr, append(s.DiagnosticValues(), seen...)...)
	}()
	if err := s.validate(); err != nil {
		return err
	}
	// One database handle serves the whole wait. Rebuilding the pool per
	// attempt also rebuilt connectionOpener and its cleanup goroutine; the
	// idle limit below still prevents a probe from reusing a stale
	// connection or driver state across readiness checks.
	var db *sql.DB
	dbDSN := ""
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	return poll(ctx, s.options, target, fmt.Sprintf("wait for SQL on %q", s.port), func(ctx context.Context) error {
		endpoint, err := target.Endpoint(ctx, s.port)
		if err != nil {
			return err
		}
		host, port, err := splitSQLEndpoint(endpoint)
		if err != nil {
			return invalidConfigf("ForSQL", "endpoint", endpoint, "is not host:port: %v", err)
		}
		dsn := s.dsn(host, port)
		addSeen(sqlDSNDiagnosticValues(dsn))
		if db == nil || dsn != dbDSN {
			if db != nil {
				_ = db.Close()
			}
			db, err = sqlOpen(s.driver, dsn)
			if err != nil {
				// The driver name is checked in validate, so a failure here
				// is the driver rejecting the DSN.
				db = nil
				return &ConfigError{
					Strategy: "ForSQL",
					Field:    "dsn",
					Reason:   err.Error(),
				}
			}
			// A single connection at a time, released after each attempt,
			// matches the previous one-pool-per-attempt lifecycle without
			// recreating the pool.
			db.SetMaxOpenConns(1)
			db.SetMaxIdleConns(0)
			dbDSN = dsn
		}
		attemptCtx, cancel := context.WithTimeout(ctx, sqlAttemptTimeout)
		defer cancel()
		if s.query == "" {
			return db.PingContext(attemptCtx)
		}
		rows, err := db.QueryContext(attemptCtx, s.query)
		if err != nil {
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		return rows.Err()
	})
}

// splitSQLEndpoint splits the target's dialable endpoint into the host and
// numeric port a DSN factory expects.
func splitSQLEndpoint(endpoint string) (string, int, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", 0, fmt.Errorf("port %q is not a number", portText)
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("port %d is out of range", port)
	}
	return host, port, nil
}

// sqlDSNDiagnosticValues returns the DSN and the password-derived forms a
// driver may echo in a connection error. Only forms recoverable with the
// standard library are covered: URL userinfo and password, URL query password
// parameters, and key=value password fields. Key=value fields whose value
// contains whitespace are recovered only on a best-effort basis.
func sqlDSNDiagnosticValues(dsn string) []string {
	if dsn == "" {
		return nil
	}
	seen := make(map[string]struct{}, 8)
	values := make([]string, 0, 8)
	add := func(value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	add(dsn)
	if parsed, err := url.Parse(dsn); err == nil {
		if parsed.User != nil {
			add(parsed.User.String())
			if password, ok := parsed.User.Password(); ok {
				add(password)
			}
		}
		query := parsed.Query()
		for _, key := range []string{"password", "passwd", "pwd"} {
			add(query.Get(key))
		}
	}
	for _, field := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "password", "passwd", "pwd":
			add(strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
	return values
}
