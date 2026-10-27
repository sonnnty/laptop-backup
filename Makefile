BIN := bin/laptop-backup

.PHONY: build test fmt install clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BIN) ./cmd/laptop-backup

test:
	gofmt -l cmd internal | (! grep .)
	go vet ./...
	go test ./...

fmt:
	gofmt -w cmd internal

install:
	./install.sh

clean:
	rm -rf bin
