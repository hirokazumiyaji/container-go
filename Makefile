.PHONY: test vet lint integration integration-docker bench-integration

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
