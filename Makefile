VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X gs3/internal/command.Version=$(VERSION)
GOFLAGS := -trimpath

REF_IMAGE := gs3-awscli-ref:1.31.13

.PHONY: build build-linux compat test lint fmt ref-image ref-dump s3fake clean

## build: static binary at bin/gs3
build:
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/gs3 ./cmd/gs3

## build-linux: Linux binary for the container the compat tests run in
build-linux:
	CGO_ENABLED=0 GOOS=linux go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o bin/gs3-linux ./cmd/gs3

## compat: differential tests against AWS CLI 1.31.13 (needs `make s3fake` running)
compat: build-linux ref-image
	python3 test/compat/compare.py test/compat/cases/*.json

## test: unit tests with the race detector
test:
	go test -race ./...

lint:
	go vet ./...
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

fmt:
	gofmt -w .

## ref-image: AWS CLI 1.31.13, the behaviour gs3 is compared against
ref-image:
	docker build -q -t $(REF_IMAGE) deploy/awscli-ref

## ref-dump: regenerate the reference data the parser tests compare against
ref-dump: ref-image
	docker run --rm --entrypoint python3 $(REF_IMAGE) /usr/local/bin/dump_argtable.py \
		> test/compat/argtable-1.31.13.json
	docker run --rm -i --entrypoint python3 $(REF_IMAGE) /usr/local/bin/dump_parse.py \
		< test/compat/parse_cases.json > test/compat/parse_expected.json

## s3fake: run the in-memory S3 server used by the tests
s3fake:
	go run ./tools/s3fake

clean:
	rm -rf bin dist
