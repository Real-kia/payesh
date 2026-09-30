.PHONY: build build-modules test lint service-check build-matrix contract-check web-check install-acceptance resource-benchmark monitoring-soak monitoring-soak-ci billing-compare release-package release-sign release-validate

TARGETS := payesh-agent payesh-server payesh payesh-privd payesh-install payesh-updater-watchdog

build:
	@set -e; \
	mkdir -p dist; \
	for target in $(TARGETS); do \
		go build -trimpath -o dist/$$target ./cmd/$$target || exit 1; \
	done

build-modules:
	@set -e; \
	mkdir -p dist/modules; \
	go build -trimpath -o dist/modules/bandwidth-controls ./cmd/payesh-bandwidth-module; \
	go build -trimpath -o dist/modules/cpu-controls ./cmd/payesh-cpu-module; \
	go build -trimpath -o dist/modules/port-traffic ./cmd/payesh-port-traffic; \
	go build -trimpath -o dist/modules/process-monitoring ./cmd/payesh-process-module

test:
	go test ./...

lint: service-check
	test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"
	go vet ./...

service-check:
	test -s deploy/systemd/payesh-agent.service
	test -s deploy/systemd/payesh-updater-watchdog.service
	test -s deploy/systemd/payesh-server.service
	test -s deploy/systemd/payesh-bandwidth-module@.service
	test -s deploy/systemd/payesh-bandwidth-watchdog@.service
	test -s deploy/systemd/payesh-cpu-controls@.service
	test -s deploy/systemd/payesh-port-traffic@.service
	test -x deploy/openrc/payesh-agent
	test -x deploy/openrc/payesh-updater-watchdog
	test -x deploy/openrc/payesh-server
	test -x deploy/openrc/payesh-bandwidth-module
	test -x deploy/openrc/payesh-bandwidth-watchdog
	test -x deploy/openrc/payesh-cpu-controls
	test -x deploy/openrc/payesh-port-traffic
	sh -n deploy/openrc/payesh-agent
	sh -n deploy/openrc/payesh-updater-watchdog
	sh -n deploy/openrc/payesh-server
	sh -n deploy/openrc/payesh-bandwidth-module
	sh -n deploy/openrc/payesh-bandwidth-watchdog
	sh -n deploy/openrc/payesh-cpu-controls
	sh -n deploy/openrc/payesh-port-traffic
	test -s deploy/logrotate.d/payesh

build-matrix:
	@set -e; \
	mkdir -p dist/matrix; \
	for arch in amd64 arm64; do \
		for target in $(TARGETS); do \
			GOOS=linux GOARCH=$$arch go build -trimpath -o dist/matrix/$$target-linux-$$arch ./cmd/$$target || exit 1; \
		done; \
		GOOS=linux GOARCH=$$arch go build -trimpath -o dist/matrix/process-monitoring-linux-$$arch ./cmd/payesh-process-module || exit 1; \
		GOOS=linux GOARCH=$$arch go build -trimpath -o dist/matrix/cpu-controls-linux-$$arch ./cmd/payesh-cpu-module || exit 1; \
		GOOS=linux GOARCH=$$arch go build -trimpath -o dist/matrix/port-traffic-linux-$$arch ./cmd/payesh-port-traffic || exit 1; \
		GOOS=linux GOARCH=$$arch go build -trimpath -o dist/matrix/bandwidth-controls-linux-$$arch ./cmd/payesh-bandwidth-module || exit 1; \
	done

contract-check:
	node scripts/contract-check.mjs

web-check:
	cd web && npm test && npm run check && npm run build

# Runs only disposable filesystem-root installer acceptance; no supervisor or
# host paths are changed. Live clean-host acceptance remains a separate gate.
install-acceptance:
	scripts/install-acceptance.sh

# Bounded local ingestion/retention evidence. Override SOAK_ARGS for a longer
# run; the command always emits a machine-readable JSON report on stdout.
monitoring-soak:
	go run ./scripts/monitoring-soak $(SOAK_ARGS)

# Short smoke for CI and pre-commit acceptance.
monitoring-soak-ci:
	go run ./scripts/monitoring-soak -duration=20s -interval=250ms -prune-interval=2s -retention-age=10s -quiet

billing-compare:
	go run ./scripts/billing-compare $(BILLING_COMPARE_ARGS)

# Bounded PID/process-tree resource measurement. The command after -- is
# intentionally caller supplied; reports are JSON by default.
resource-benchmark:
	scripts/resource-benchmark.sh $(ARGS)

# Produces deterministic Linux amd64/arm64 archives plus an unsigned local
# manifest and SHA256SUMS. Release signing remains an explicit owner action;
# see docs/CONTRIBUTING.md.
release-package:
	go run ./scripts/release-package

release-sign:
	@test -n "$(RELEASE_SIGNING_KEY)" || (echo "RELEASE_SIGNING_KEY is required" >&2; exit 2)
	@test -n "$(RELEASE_SIGNING_KEY_ID)" || (echo "RELEASE_SIGNING_KEY_ID is required" >&2; exit 2)
	go run ./scripts/release-sign -dir $(if $(RELEASE_DIR),$(RELEASE_DIR),dist/releases/$(if $(RELEASE_VERSION),$(RELEASE_VERSION),0.1.0)) -key "$(RELEASE_SIGNING_KEY)" -key-id "$(RELEASE_SIGNING_KEY_ID)"

release-validate:
	go run ./scripts/release-validate -dir $(if $(RELEASE_DIR),$(RELEASE_DIR),dist/releases/$(if $(RELEASE_VERSION),$(RELEASE_VERSION),0.1.0)) $(if $(RELEASE_PUBLIC_KEY),-public-key "$(RELEASE_PUBLIC_KEY)") $(if $(RELEASE_SIGNING_KEY_ID),-key-id "$(RELEASE_SIGNING_KEY_ID)")
