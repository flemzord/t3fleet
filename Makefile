.PHONY: build fmt lint test check
build:
	go build -trimpath -o bin/t3fleet ./cmd/t3fleet
fmt:
	gofmt -w $$(find . -name '*.go' -not -path './vendor/*')
lint:
	@test -z "$$(gofmt -l $$(find . -name '*.go' -not -path './vendor/*'))" || { echo 'Run make fmt'; exit 1; }
	go vet ./...
test:
	go test -race -count=1 ./...
check: lint test
