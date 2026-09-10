.PHONY: build test lint service-check build-matrix contract-check web-check

TARGETS := payesh-agent payesh-server payesh payesh-privd

build:
	@set -e; \
	mkdir -p dist; \
	for target in $(TARGETS); do \
		go build -trimpath -o dist/$$target ./cmd/$$target || exit 1; \
	done

test:
	go test ./...

lint: service-check
	test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))"
	go vet ./...

service-check:
	test -s deploy/systemd/payesh-agent.service
	test -s deploy/systemd/payesh-server.service
	test -x deploy/openrc/payesh-agent
	test -x deploy/openrc/payesh-server
	sh -n deploy/openrc/payesh-agent
	sh -n deploy/openrc/payesh-server
	test -s deploy/logrotate.d/payesh

build-matrix:
	@set -e; \
	mkdir -p dist/matrix; \
	for arch in amd64 arm64; do \
		for target in $(TARGETS); do \
			GOOS=linux GOARCH=$$arch go build -trimpath -o dist/matrix/$$target-linux-$$arch ./cmd/$$target || exit 1; \
		done; \
	done

contract-check:
	node scripts/contract-check.mjs

web-check:
	cd web && npm run check && npm run build
