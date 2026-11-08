BINARY  := fastrtmp
VERSION := 1.0.0
LDFLAGS := -s -w -X main.version=$(VERSION)
GOFLAGS := CGO_ENABLED=0

.PHONY: all build test bench run clean linux windows darwin fmt vet

all: build

build:
	$(GOFLAGS) go build -ldflags="$(LDFLAGS)" -o bin/$(BINARY) ./cmd/fastrtmp
	$(GOFLAGS) go build -ldflags="$(LDFLAGS)" -o bin/rtmptest ./cmd/rtmptest

test:
	go test ./... -count=1

fmt:
	gofmt -l -w .

vet:
	go vet ./...

bench:
	go test ./internal/rtmp -run TestPushPull -v -count=1

run: build
	./bin/$(BINARY) -addr :1935 -http :8080

# Cross compilation: one source tree, Windows and Linux binaries.
linux:
	$(GOFLAGS) GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/fastrtmp-linux-amd64 ./cmd/fastrtmp
	$(GOFLAGS) GOOS=linux GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/fastrtmp-linux-arm64 ./cmd/fastrtmp

windows:
	$(GOFLAGS) GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o bin/fastrtmp-windows-amd64.exe ./cmd/fastrtmp

darwin:
	$(GOFLAGS) GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o bin/fastrtmp-darwin-arm64 ./cmd/fastrtmp

clean:
	rm -rf bin

.PHONY: release
release: linux windows darwin
