package examples

import (
	"context"
	"crypto/tls"
	"net/http"
	"testing"
	"time"

	container "github.com/hirokazumiyaji/container-go"
	"github.com/hirokazumiyaji/container-go/wait"
)

// TestDocumentationExamplesCompile keeps the public calls used by the
// README and design document type-checked without requiring a backend.
func TestDocumentationExamplesCompile(t *testing.T) {
	t.Skip("compile-only check; run the integration examples for a backend")
	compileDocumentationExamples(t, context.Background())
}

func compileDocumentationExamples(t *testing.T, ctx context.Context) {
	ctr, err := container.Run(ctx, "redis:7-alpine",
		container.WithExposedPorts("6379/tcp"),
		container.WithEnv(map[string]string{"REDIS_ARGS": "--appendonly yes"}),
		container.WithWaitStrategy(wait.ForListeningPort("6379/tcp")),
	)
	container.Cleanup(t, ctr)
	if err != nil {
		t.Fatal(err)
	}

	endpoint, err := ctr.Endpoint(ctx, "6379/tcp")
	if err != nil {
		t.Fatal(err)
	}
	_ = endpoint

	_ = container.WithPullPolicy(container.PullAlways)
	_ = container.WithName("docs-example")
	_ = container.WithReuse()
	_ = container.WithReuseGroup("docs")
	_ = container.WithPublishedPort("127.0.0.1:18080:80")
	_ = container.WithExecEnv(map[string]string{"MODE": "test"})
	_ = container.WithExecUser("test")
	_ = container.WithExecWorkDir("/tmp")
	_ = container.Pull(ctx, "redis:7-alpine")

	logs, err := ctr.LogsWithOptions(ctx, container.LogsOptions{
		Tail:  10,
		Since: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = logs.Close()

	stream, err := ctr.FollowLogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()

	_ = wait.ForLog("Ready").AsRegexp().WithOccurrence(1).
		WithStartupTimeout(time.Second).WithPollInterval(time.Millisecond)
	_ = wait.ForExposedPort().WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond)
	_ = wait.ForHTTP("/health").WithPort("6379/tcp").WithMethod(http.MethodGet).
		WithStatusCodeMatcher(func(status int) bool { return status == http.StatusOK }).
		WithHeaders(map[string]string{"X-Test": "yes"}).WithHeader("X-Other", "yes").
		WithBasicAuth("user", "pass").WithTLSConfig(&tls.Config{}).
		WithHTTPClient(&http.Client{}).WithStartupTimeout(time.Second).
		WithPollInterval(time.Millisecond)
	_ = wait.ForExec([]string{"true"}).WithExitCodeMatcher(func(code int) bool { return code == 0 }).
		WithStartupTimeout(time.Second).WithPollInterval(time.Millisecond)
	_ = wait.ForAll(wait.ForExposedPort()).WithStartupTimeout(time.Second)
	_ = wait.ForAny(wait.ForExposedPort()).WithStartupTimeout(time.Second)
}
