.PHONY: build run docs tidy clean

# Build the single static binary.
build:
	go build -o webrudder .

# Build and run. Flags must precede the URL (Go's flag package stops at the
# first positional arg). Override: make run URL=https://example.com PORT=10000
URL ?=
PORT ?= 10000
run: build
	./webrudder --port $(PORT) $(URL)

# Regenerate the embedded OpenAPI spec from handler annotations.
# Requires: go install github.com/swaggo/swag/cmd/swag@latest
docs:
	swag init -g main.go --parseInternal -o docs

tidy:
	go mod tidy

clean:
	rm -f webrudder
