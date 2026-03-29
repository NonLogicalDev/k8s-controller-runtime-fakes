export SOURCE_DATE_EPOCH := 1

.PHONY: build test

build:
	go build ./...

test:
	go test ./...
