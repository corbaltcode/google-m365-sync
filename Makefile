.PHONY: build test zip clean

BIN := dist/bootstrap
ZIP := dist/google-m365-sync.zip

test:
	go vet ./...
	go test ./...
	go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...

build:
	mkdir -p dist
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o $(BIN) .

zip: build
	cd dist && rm -f google-m365-sync.zip && zip -q -X google-m365-sync.zip bootstrap

clean:
	rm -rf dist
