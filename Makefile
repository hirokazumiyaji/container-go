.PHONY: test vet vet-integration lint deadcode govulncheck govulncheck-bench actionlint integration integration-docker bench-integration release-check release-check-bench

test:
	go test ./...

vet:
	go vet ./...

# Every gate that can compile the `integration`-tagged files type-checks them
# with this command. Go ignores an unknown build tag silently, so a rename that
# misses one copy would quietly stop checking instead of failing.
VET_INTEGRATION := go vet -tags integration ./...

vet-integration:
	$(VET_INTEGRATION)
	cd bench && $(VET_INTEGRATION)

lint:
	golangci-lint run ./...

# deadcode reports functions that no main or test binary reaches. Some
# exported entry points are intentionally only callable by library users, so
# they never appear reachable from in-repo code; these lists name exactly those.
# The internal/bench scenarios are the compatibility helpers that predate this
# guard. Anything else is a regression: a forwarding wrapper left behind, or an
# unexported helper kept alive only by another unreachable function.
#
# bench/ is a separate module that a root-level ./... never reaches, so it is
# checked on its own. Its result-schema helpers are documented API
# (docs/benchmarks.md) that only an external benchmark consumer calls.
#
# The report is reduced to the symbol and matched with `grep -Evx`, so an
# allowed name has to equal the whole unreachable symbol; a plain substring
# match would let a new longer name hide behind an allowlisted prefix.
#
# Build from tools/go.mod so the tool has one pinned version and Dependabot can
# update it; `go run ...@latest` would float and an unpinned download in CI
# would be a supply-chain hole.
DEADCODE_ALLOWED := ScenarioPolicies|ScenarioPolicyFor|ScenarioNames
DEADCODE_ALLOWED_BENCH := ScenarioPolicies|ScenarioNames|ScenarioPolicyFor|ScenarioPoliciesFor|CurrentCommit|CurrentSource|RequireCleanSource|ValidateScenarioSet|CompareDocs|GenerateBaselineProvenance|NormalizeEnv|NormalizeDoc

deadcode:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	(cd tools && go build -o "$$tmp/deadcode" golang.org/x/tools/cmd/deadcode) || exit $$?; \
	"$$tmp/deadcode" -test -tags integration ./... > "$$tmp/root.out" || exit $$?; \
	(cd bench && "$$tmp/deadcode" -test -tags integration ./... > "$$tmp/bench.out") || exit $$?; \
	check() { \
		label="$$1"; out="$$2"; allowed="$$3"; \
		if [ -z "$$out" ]; then echo "deadcode ($$label): no unreachable functions"; return 0; fi; \
		unexpected=$$(printf '%s\n' "$$out" | sed -E 's/^.*: unreachable func: //' | grep -Evx "($$allowed)" || true); \
		if [ -n "$$unexpected" ]; then \
			printf '%s\n' "$$out"; \
			echo "error: unreachable functions beyond the documented exported API in the $$label" >&2; \
			return 1; \
		fi; \
		echo "deadcode ($$label): only documented exported API functions are unreachable"; \
	}; \
	check 'root module' "$$(cat "$$tmp/root.out")" '$(DEADCODE_ALLOWED)' && \
	check 'bench module' "$$(cat "$$tmp/bench.out")" '$(DEADCODE_ALLOWED_BENCH)'

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

# This target intentionally includes the pull-heavy singleflight case;
# keep its root-module command in sync with docs/benchmarks.md.
bench-integration:
	go test -tags integration -count=1 -timeout 30m -run 'TestIntegrationBench|TestIntegrationPullSingleflight' ./...
	cd bench && go test -tags integration -count=1 -timeout 30m ./...

# Everything a tag must satisfy that can be checked without a container
# backend, runnable from a clean checkout before tagging.
#
# This target is the gate, and the release-check workflow calls it rather than
# restating the steps. Two parallel lists of checks drift, and the weaker one is
# the one a maintainer runs locally before tagging.
#
# The `integration`-tagged suites are deliberately absent: they need a real
# backend, which a hosted runner does not have for either backend, and they run
# locally via `make integration` / `make bench-integration`. They are not
# ignored, though — `go vet -tags integration` type-checks them below, so a
# rename that breaks their compilation fails the gate even though they are not
# executed.
#
# `go mod tidy -diff` reports what tidy would change and exits non-zero without
# writing anything. Applying tidy and diffing afterwards would also miss a
# go.sum that tidy creates from nothing, because `git diff` ignores untracked
# files, and a failing run would leave a rewritten, unreviewed go.mod in the
# tree.
# govulncheck and actionlint are built from tools/go.mod, not run as
# `go run pkg@version`: a versioned run ignores the module in the current
# directory, so nothing verifies the downloaded tree against tools/go.sum and
# the tool CI executes need not be the one Dependabot updates. The version
# lives only in tools/go.mod; its tests check that no copy returns.
GOVULNCHECK_TOOL := golang.org/x/vuln/cmd/govulncheck
ACTIONLINT_TOOL := github.com/rhysd/actionlint/cmd/actionlint

govulncheck:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	(cd tools && go build -o "$$tmp/govulncheck" $(GOVULNCHECK_TOOL)) || exit $$?; \
	"$$tmp/govulncheck" ./...

govulncheck-bench:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	(cd tools && go build -o "$$tmp/govulncheck" $(GOVULNCHECK_TOOL)) || exit $$?; \
	cd bench && "$$tmp/govulncheck" ./...

# shellcheck is left enabled: actionlint uses it when installed, and the runner
# image and mise both provide it. Disabling it would check the run: blocks only
# for Actions expressions and miss ordinary shell bugs.
actionlint:
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	(cd tools && go build -o "$$tmp/actionlint" $(ACTIONLINT_TOOL)) || exit $$?; \
	"$$tmp/actionlint"

release-check:
	@command -v golangci-lint >/dev/null || { \
		echo 'error: golangci-lint not found; run "mise install" (see AGENTS.md)' >&2; exit 1; }
	go build ./...
	# Also type-checks the `integration`-tagged files, which no untagged build
	# compiles. ci.yml's integration-docker job covers this on a PR, but that
	# job does not run on a tag push, so the tag gate would not.
	$(VET_INTEGRATION)
	$(MAKE) lint
	$(MAKE) deadcode
	go test -count=1 -race ./...
	go mod verify
	go mod tidy -diff
	$(MAKE) govulncheck
	$(MAKE) release-check-bench
	cd tools && go mod verify && go mod tidy -diff
	$(MAKE) actionlint

# bench/ is a separate module, so a root-level ./... reaches none of it: not
# the build, and not golangci-lint or govulncheck either. Verified by planting a
# violation in bench and watching a root `golangci-lint run ./...` report
# "0 issues". So every check the root gets, bench gets again, from inside bench.
#
# The untagged `go test` is not at parity with the root's, and is not meant to
# be: both real bench tests are `//go:build integration` and need a backend, so
# untagged this module runs exactly one test, a fixture-schema check. The tagged
# vet above is what keeps the other two from bit-rotting uncompiled.
release-check-bench:
	cd bench && go build ./... \
		&& go vet ./... \
		&& $(VET_INTEGRATION) \
		&& golangci-lint run ./... \
		&& go test -count=1 -race ./... \
		&& go mod verify \
		&& go mod tidy -diff
	$(MAKE) govulncheck-bench

