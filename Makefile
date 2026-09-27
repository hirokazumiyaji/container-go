.PHONY: test vet lint integration integration-docker bench-integration release-check

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

# Skip pull-heavy bench / singleflight cases here; use bench-integration.
integration:
	go test -tags integration -count=1 -timeout 20m -skip 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...

# `go test` exits 0 when a -run pattern matches nothing, so assert on the
# verbose log that the pattern actually selected a test. Every non-matching
# package prints "no tests to run", so the aggregate string is useless; a
# `=== RUN TestIntegrationDocker...` line appears only for a test that ran.
#
# Under REQUIRE_BACKEND the job must also have exercised the backend, so a
# pass is required rather than a mere start. Locally a missing daemon is a
# legitimate skip, so only the "selected nothing" case fails there.
#
# The output is streamed with tee rather than buffered and cat-ed: the job
# timeout can fire while the test binary is still running, and buffering
# would discard the log in exactly that case. `go test`'s own status cannot
# be read from a POSIX pipeline, so it is written to a side file.
integration-docker:
	@log=$$(mktemp); st=$$(mktemp); \
	{ go test -tags integration -count=1 -timeout 20m -v -run IntegrationDocker ./... 2>&1; echo $$? >"$$st"; } | tee "$$log"; \
	status=$$(cat "$$st"); \
	rm -f "$$st"; \
	if grep -qE '^FAIL[[:space:]]+\S+[[:space:]]+\[build failed\]' "$$log"; then \
		echo 'error: the integration suite failed to build; see the compiler output above'; \
		status=1; \
	elif ! grep -qE '^=== RUN[[:space:]]+TestIntegrationDocker' "$$log"; then \
		echo 'error: -run IntegrationDocker selected no tests; check the test names and the integration build tag'; \
		status=1; \
	elif [ "$$REQUIRE_BACKEND" = 1 ] && ! grep -qE '^--- PASS:[[:space:]]+TestIntegrationDocker' "$$log"; then \
		echo 'error: no TestIntegrationDocker test passed; see the failures above (backend unavailable?)'; \
		status=1; \
	fi; \
	rm -f "$$log"; \
	exit $$status

bench-integration:
	go test -tags integration -count=1 -timeout 30m -run 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...
	cd bench && go test -tags integration -count=1 -timeout 30m ./...

# Everything a tag must satisfy, runnable from a clean checkout before tagging.
#
# This target is the gate, and the release-check workflow calls it rather than
# restating the steps. Two parallel lists of checks drift, and the weaker one is
# the one a maintainer runs locally before tagging.
#
# `go mod tidy -diff` reports what tidy would change and exits non-zero without
# writing anything. Applying tidy and diffing afterwards would also miss a
# go.sum that tidy creates from nothing, because `git diff` ignores untracked
# files, and a failing run would leave a rewritten, unreviewed go.mod in the
# tree.
#
# bench/ is a separate module, so a root-level ./... never reaches it. It gets
# its own full pass, including the tidy check.
GOVULNCHECK_VERSION := v1.1.4
ACTIONLINT_VERSION := v1.7.12

release-check:
	@command -v golangci-lint >/dev/null || { \
		echo 'error: golangci-lint not found; run "mise install" (see AGENTS.md)' >&2; exit 1; }
	go build ./...
	go vet ./...
	$(MAKE) lint
	go test -count=1 -race ./...
	go mod verify
	go mod tidy -diff
	go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION) ./...
	cd bench && go build ./... && go vet ./... && go test -count=1 -race ./... \
		&& go mod verify && go mod tidy -diff
	go run github.com/rhysd/actionlint/cmd/actionlint@$(ACTIONLINT_VERSION) -shellcheck= -pyflakes=

