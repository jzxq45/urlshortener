BINARY = bin/server

.PHONY: build test vet fmt lint run tidy clean

build:
	go build -o $(BINARY) ./cmd/server

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

lint:
	golangci-lint run

run:
	go run ./cmd/server

tidy:
	go mod tidy

clean:
	go clean
	rm -rf bin
