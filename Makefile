.PHONY: build test clean lint vet

BINARY=mcp-mux
GOFLAGS=-trimpath

build:
	go build $(GOFLAGS) -o $(BINARY).exe ./cmd/mcp-mux

test:
	go test -race -count=1 ./...

test-v:
	go test -race -count=1 -v ./...

vet:
	go vet ./...

lint: vet
	@echo "Lint passed (go vet)"

clean:
	rm -f $(BINARY).exe $(BINARY)

cover:
	go test -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
