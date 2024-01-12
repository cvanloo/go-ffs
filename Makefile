.PHONY: test

test:
	@go test ./...

help:
	@cat Makefile | grep -E "^\w+:$:"
