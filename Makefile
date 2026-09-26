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

# A required integration job must actually exercise the backend. `go test`
# exits 0 when a -run pattern matches nothing, so assert on the verbose log:
# every non-matching package prints "no tests to run", so the aggregate
# string is useless. A `--- PASS: TestIntegrationDocker...` line appears only
# for a test that started and passed, which is what we need.
integration-docker:
	@log=$$(mktemp); \
	trap 'rm -f "$$log"' EXIT INT TERM; \
	go test -tags integration -count=1 -timeout 20m -v -run IntegrationDocker ./... >$$log 2>&1; \
	status=$$?; \
	cat $$log; \
	if ! grep -qE '^--- PASS:[[:space:]]+TestIntegrationDocker' $$log; then \
		echo 'error: no TestIntegrationDocker test passed; the pattern matched nothing or every test skipped'; \
		status=1; \
	fi; \
	rm -f $$log; trap - EXIT INT TERM; \
	exit $$status

bench-integration:
	go test -tags integration -count=1 -timeout 30m -run 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...
	cd bench && go test -tags integration -count=1 -timeout 30m ./...
