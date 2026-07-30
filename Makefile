# Get the latest commit branch, hash, and date
TAG=$(shell git describe --tags --abbrev=0 --exact-match 2>/dev/null)
BRANCH=$(if $(TAG),$(TAG),$(shell git rev-parse --abbrev-ref HEAD 2>/dev/null))
HASH=$(shell git rev-parse --short=7 HEAD 2>/dev/null)
TIMESTAMP=$(shell git log -1 --format=%ct HEAD 2>/dev/null | xargs -I{} date -u -r {} +%Y%m%dT%H%M%S)
GIT_REV=$(shell printf "%s-%s-%s" "$(BRANCH)" "$(HASH)" "$(TIMESTAMP)")
REV=$(if $(filter --,$(GIT_REV)),latest,$(GIT_REV))

BIN=tg-mcp
SRC=./cmd/$(BIN)

all: test build

build:
	go build -ldflags "-X main.revision=$(REV) -s -w" -o .bin/$(BIN).$(BRANCH) $(SRC)
	cp .bin/$(BIN).$(BRANCH) .bin/$(BIN)

test:
	go clean -testcache
	go test -race -coverprofile=coverage.out ./...
	grep -v "_mock.go" coverage.out | grep -v mocks > coverage_no_mocks.out
	go tool cover -func=coverage_no_mocks.out
	rm coverage.out coverage_no_mocks.out

e2e:
	go test -tags=e2e -race -count=1 ./...

lint:
	golangci-lint run

fmt:
	gofmt -s -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")
	goimports -w $$(find . -type f -name "*.go" -not -path "./vendor/*" -not -path "./mocks/*" -not -path "**/mocks/*")

race:
	go test -race -timeout=60s ./...

version:
	@echo "branch: $(BRANCH), hash: $(HASH), timestamp: $(TIMESTAMP)"
	@echo "revision: $(REV)"

.PHONY: all build test e2e lint fmt race version
