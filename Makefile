GO ?= go
PREFIX ?= /usr/local
DESTDIR ?=
BINDIR ?= $(PREFIX)/bin
DATADIR ?= $(PREFIX)/share/craftmake
BUILD_DIR ?= build
BINARY ?= $(BUILD_DIR)/craftmake
DIST_DIR ?= dist
VERSION ?= 0.1.0-dev
COMMIT ?= unknown
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
GO_LDFLAGS ?= -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(BUILD_DATE)
RELEASE_PLATFORMS ?= linux/amd64 linux/arm64

.PHONY: all build install uninstall release release-archive benchmark-pdx-scheduler test vet check clean

all: build

build:
	mkdir -p "$(BUILD_DIR)"
	$(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o "$(BINARY)" ./cmd/craftmake

install: build
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 "$(BINARY)" "$(DESTDIR)$(BINDIR)/craftmake"
	install -d "$(DESTDIR)$(DATADIR)/workflows"
	cp -R workflows/. "$(DESTDIR)$(DATADIR)/workflows/"
	install -d "$(DESTDIR)$(DATADIR)/configs"
	cp -R configs/. "$(DESTDIR)$(DATADIR)/configs/"
	chmod -R a+rX "$(DESTDIR)$(DATADIR)/workflows" "$(DESTDIR)$(DATADIR)/configs"

uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/craftmake"
	rm -rf "$(DESTDIR)$(DATADIR)/workflows"
	rmdir --ignore-fail-on-non-empty "$(DESTDIR)$(DATADIR)" 2>/dev/null || true

release:
	rm -rf "$(DIST_DIR)"
	mkdir -p "$(DIST_DIR)"
	$(MAKE) release-archive RELEASE_GOOS=linux RELEASE_GOARCH=amd64
	$(MAKE) release-archive RELEASE_GOOS=linux RELEASE_GOARCH=arm64
	cd "$(DIST_DIR)" && sha256sum *.tar.gz > checksums.txt

release-archive:
	mkdir -p "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)/bin"
	mkdir -p "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)/share/craftmake/workflows"
	mkdir -p "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)/share/craftmake/configs"
	CGO_ENABLED=0 GOOS="$(RELEASE_GOOS)" GOARCH="$(RELEASE_GOARCH)" $(GO) build -trimpath -ldflags "$(GO_LDFLAGS)" -o "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)/bin/craftmake" ./cmd/craftmake
	cp -R workflows/. "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)/share/craftmake/workflows/"
	cp -R configs/. "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)/share/craftmake/configs/"
	chmod -R a+rX "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)"
	tar -C "$(DIST_DIR)" -czf "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH).tar.gz" "craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)"
	rm -rf "$(DIST_DIR)/craftmake_$(VERSION)_$(RELEASE_GOOS)_$(RELEASE_GOARCH)"

benchmark-pdx-scheduler:
	python3 scripts/generate_pdx_scheduler_benchmark.py \
		--source doc/benchmarks/gate6-r41-pdx-scheduling-repeat-aggregate-r2.source.json \
		--summary-csv doc/benchmarks/pdx-step2-check-controller-reconciliation-summary.csv \
		--figure-svg doc/benchmarks/pdx-step2-check-controller-reconciliation.svg

test:
	$(GO) test ./... -count=1

vet:
	$(GO) vet ./...

check: test vet

clean:
	rm -rf "$(BUILD_DIR)" "$(DIST_DIR)"
