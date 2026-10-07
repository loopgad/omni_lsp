# Local entry point for the checks that otherwise have to be typed one at a time.
# Windows has no `make` by default; use `mingw32-make test` there.
#
#   make test     gofmt, build, vet, tests, verify
#   make fast     formatting, build, vet and verify only
#   make race     the mode CI runs (race detector)
#   make short    skip the packages that shell out to language toolchains
#   make clean-dist  remove omnilsp-$(VERSION)-* build artifacts from $(DIST)
#
# Every recipe is a plain `go` invocation, so nothing needs installing.
#
# Not covered here: test/soak, which is behind the `soak` build tag and runs on
# its own schedule -- see the nightly workflow for `go test -tags soak`.

GO ?= go
TIMEOUT ?= 90m
VERSION ?= dev
DIST ?= dist

.PHONY: test fast race short fmt vet build verify clean-dist

test: ## gofmt, build, vet, tests, verify
	$(MAKE) fmt vet build
	$(GO) test -count=1 -timeout $(TIMEOUT) ./...
	$(MAKE) verify

fast: ## formatting, build, vet and verify only; no test run
	$(MAKE) fmt vet build verify

race: ## the mode CI runs
	$(GO) test -race -count=1 -timeout $(TIMEOUT) ./...

short: ## skip the packages that shell out to language toolchains
	$(GO) test -short -count=1 -timeout $(TIMEOUT) ./...

fmt: ## fail when repository source is unformatted
	@unformatted=$$(gofmt -l internal cmd test 2>/dev/null | grep -v evidence); \
	if [ -n "$$unformatted" ]; then echo "$$unformatted"; exit 1; fi

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

verify: ## the binary CI ships; also resolves every conformance probe symbol
	$(GO) run ./cmd/omnilsp verify --min 90

clean-dist: ## delete this version's artifacts from $(DIST); same narrow scope as build-multiplatform.sh
	@if [ -d "$(DIST)" ]; then \
		find "$(DIST)" -maxdepth 1 -type f -name "omnilsp-$(VERSION)-*" -delete; \
		echo "cleaned omnilsp-$(VERSION)-* from $(DIST)"; \
	else \
		echo "$(DIST) does not exist; nothing to clean"; \
	fi