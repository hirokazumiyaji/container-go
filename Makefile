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

integration-docker:
	@log=$$(mktemp); \
	go test -tags integration -count=1 -timeout 20m -v -run IntegrationDocker ./... 2>&1 | tee $$log; \
	status=$${PIPESTATUS[0]}; \
	grep -q 'no tests to run' $$log && { echo 'error: -run IntegrationDocker selected no tests'; status=1; }; \
	rm -f $$log; \
	exit $$status

bench-integration:
	go test -tags integration -count=1 -timeout 30m -run 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...
	cd bench && go test -tags integration -count=1 -timeout 30m ./...
