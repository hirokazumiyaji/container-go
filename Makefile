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
integration-docker:
	@log=$$(mktemp); \
	go test -tags integration -count=1 -timeout 20m -v -run IntegrationDocker ./... >"$$log" 2>&1; \
	status=$$?; \
	cat "$$log"; \
	if ! grep -qE '^=== RUN[[:space:]]+TestIntegrationDocker' "$$log"; then \
		echo 'error: -run IntegrationDocker selected no tests; check the test names and the integration build tag'; \
		status=1; \
	elif [ "$$REQUIRE_BACKEND" = 1 ] && ! grep -qE '^--- PASS:[[:space:]]+TestIntegrationDocker' "$$log"; then \
		echo 'error: no TestIntegrationDocker test passed; check the SKIP lines above (backend unavailable?)'; \
		status=1; \
	fi; \
	rm -f "$$log"; \
	exit $$status

bench-integration:
	go test -tags integration -count=1 -timeout 30m -run 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...
	cd bench && go test -tags integration -count=1 -timeout 30m ./...
