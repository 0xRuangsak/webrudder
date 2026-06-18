.PHONY: build docs tidy clean

# Build the single static binary.
build:
	go build -o webrudder .

# Regenerate the embedded OpenAPI spec from handler annotations.
# Requires: go install github.com/swaggo/swag/cmd/swag@latest
docs:
	swag init -g main.go --parseInternal -o docs

tidy:
	go mod tidy

clean:
	rm -f webrudder
