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

	"github.com/hirokazumiyaji/container-go/internal/cli"
)

// sqlAttemptTimeout bounds one database/sql probe attempt so a stalled
// connection handshake cannot consume the whole startup budget. Overridable in
// tests.
var sqlAttemptTimeout = 3 * time.Second

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
// together with the password-derived forms the common driver DSN dialects
// expose (URL userinfo and secret-named query parameters, MySQL
// user:password@protocol(address)/dbname, and key=value fields including
// quoted libpq and semicolon-separated ADO/ODBC values). A log tail attached
// by a higher-level helper after the wait fails is outside that redaction
// because the DSN is not known before the wait starts.
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
// during a wait and therefore stays with that wait's error. The port and
// driver are connection metadata, not secrets: registering them would turn
// ordinary diagnostics such as "invalid driver \"...\"" into redacted noise.
func (s *SQLStrategy) DiagnosticValues() []string {
	return []string{s.query}
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
		db, err := sql.Open(s.driver, dsn)
		if err != nil {
			// The driver name is checked in validate, so a failure here is
			// the driver rejecting the DSN.
			return &ConfigError{
				Strategy: "ForSQL",
				Field:    "dsn",
				Reason:   err.Error(),
			}
		}
		// One pool per attempt: a probe must not carry a stale connection or
		// driver state into the next readiness check.
		defer func() { _ = db.Close() }()
		attemptCtx, cancel := context.WithTimeout(ctx, sqlAttemptTimeout)
		defer cancel()
		if s.query == "" {
			return db.PingContext(attemptCtx)
		}
		rows, err := db.QueryContext(attemptCtx, s.query)
		if err != nil {
			return err
		}
		// Advance once so a driver that defers a failure until the first
		// row fetch reports it through rows.Err; database/sql only exposes
		// such an error after Next has moved the cursor. One advance is
		// enough for readiness, and it keeps a large or unbounded result from
		// extending the probe.
		defer func() { _ = rows.Close() }()
		rows.Next()
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
// driver may echo in a connection error. Drivers do not share one DSN grammar,
// so every dialect is scanned independently: URL DSNs, MySQL's
// user:password@protocol(address)/dbname form, semicolon-separated ADO/ODBC
// key=value pairs, and libpq key=value pairs whose values may be quoted and
// contain whitespace. Scanning the raw text keeps a malformed URL from hiding
// the password forms it still contains.
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
	addURLDSNSecrets(dsn, add)
	addMySQLDSNSecrets(dsn, add)
	addDSNFieldSecrets(dsn, add)
	return values
}

// addURLDSNSecrets registers userinfo and secret-named query values from a
// scheme://... DSN.
func addURLDSNSecrets(dsn string, add func(string)) {
	_, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return
	}
	authority, query, hasQuery := strings.Cut(rest, "?")
	if i := strings.IndexAny(authority, "/#"); i >= 0 {
		authority = authority[:i]
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		addUserInfoSecrets(authority[:at], add)
	}
	if hasQuery {
		addQueryDSNSecrets(query, add)
	}
}

// addMySQLDSNSecrets registers the password of MySQL's
// [user[:password]@][protocol[(address)]]/dbname form, which has no scheme and
// would otherwise parse as a URL with the user name as its scheme.
func addMySQLDSNSecrets(dsn string, add func(string)) {
	if strings.Contains(dsn, "://") {
		return
	}
	base, query, hasQuery := strings.Cut(dsn, "?")
	slash := strings.LastIndexByte(base, '/')
	if slash <= 0 {
		return
	}
	prefix := base[:slash]
	at := strings.LastIndexByte(prefix, '@')
	if at <= 0 {
		return
	}
	userinfo := prefix[:at]
	// A key=value or semicolon pair before the separator is another dialect,
	// not MySQL userinfo. Spaces are allowed because a MySQL password may
	// contain them.
	if strings.ContainsAny(userinfo, "=;") {
		return
	}
	addUserInfoSecrets(userinfo, add)
	if hasQuery {
		addQueryDSNSecrets(query, add)
	}
}

// addQueryDSNSecrets registers values of secret-named query parameters. Query
// keys are case-insensitive, so matching goes through cli.IsSecretName rather
// than a fixed lower-case list.
func addQueryDSNSecrets(rawQuery string, add func(string)) {
	rawQuery, _, _ = strings.Cut(rawQuery, "#")
	for _, pair := range strings.FieldsFunc(rawQuery, func(r rune) bool { return r == '&' || r == ';' }) {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || value == "" {
			continue
		}
		decodedKey, err := url.QueryUnescape(key)
		if err != nil {
			decodedKey = key
		}
		if !secretDSNKey(decodedKey) {
			continue
		}
		add(value)
		if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
			add(decoded)
		}
	}
}

// addDSNFieldSecrets registers values of secret-named key=value fields. The
// field scanner keeps quoted and braced values intact, so a password such as
// 'my secret' is registered whole rather than split into harmless words.
func addDSNFieldSecrets(dsn string, add func(string)) {
	for _, field := range splitDSNFields(dsn) {
		key, rawValue, ok := strings.Cut(field, "=")
		if !ok || !secretDSNKey(key) {
			continue
		}
		if value := dsnFieldValue(rawValue); value != "" {
			add(value)
		}
	}
}

// splitDSNFields splits a key=value DSN at unquoted whitespace and semicolons.
// Single- and double-quoted values and ODBC {braced} values keep their
// separators, and a backslash escapes the next byte inside a quoted value.
func splitDSNFields(dsn string) []string {
	var fields []string
	start := -1
	var quote byte
	braces := 0
	for i := 0; i < len(dsn); i++ {
		c := dsn[i]
		if quote != 0 {
			if c == '\\' && i+1 < len(dsn) {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if braces > 0 {
			if c == '}' {
				if i+1 < len(dsn) && dsn[i+1] == '}' {
					i++
					continue
				}
				braces--
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '{':
			braces++
		case ' ', '\t', '\r', '\n', ';':
			if start >= 0 {
				fields = append(fields, dsn[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		fields = append(fields, dsn[start:])
	}
	return fields
}

// dsnFieldValue strips the optional quoting an ADO/ODBC or libpq value may
// carry and resolves its escapes.
func dsnFieldValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) < 2 {
		return value
	}
	switch {
	case value[0] == '\'' && value[len(value)-1] == '\'':
		return unescapeDSNQuoted(value[1 : len(value)-1])
	case value[0] == '"' && value[len(value)-1] == '"':
		return unescapeDSNQuoted(value[1 : len(value)-1])
	case value[0] == '{' && value[len(value)-1] == '}':
		return strings.ReplaceAll(value[1:len(value)-1], "}}", "}")
	}
	return value
}

// unescapeDSNQuoted removes the backslash escapes libpq permits inside a
// quoted value.
func unescapeDSNQuoted(value string) string {
	var b strings.Builder
	b.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+1 < len(value) {
			i++
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

// addUserInfoSecrets registers a user:password prefix in both its escaped and
// unescaped spellings.
func addUserInfoSecrets(userinfo string, add func(string)) {
	add(userinfo)
	if unescaped, err := url.PathUnescape(userinfo); err == nil {
		add(unescaped)
	}
	_, password, ok := strings.Cut(userinfo, ":")
	if !ok {
		return
	}
	add(password)
	if unescaped, err := url.PathUnescape(password); err == nil && unescaped != password {
		add(unescaped)
	}
}

// secretDSNKey reports whether a DSN field name marks its value as a secret.
// A bare pwd is included explicitly because cli.IsSecretName treats it as the
// working directory unless it is qualified.
func secretDSNKey(key string) bool {
	key = strings.TrimSpace(key)
	if strings.EqualFold(key, "pwd") {
		return true
	}
	return cli.IsSecretName(key)
}
