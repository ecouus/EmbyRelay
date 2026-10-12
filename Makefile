.PHONY: test build run web agent clean
web:
	npm --prefix web ci
	npm --prefix web run build
test:
	go test -race ./...
	go vet ./...
build: web
	mkdir -p bin
	go build -trimpath -o bin/embyrelay ./cmd/server
	go build -trimpath -o bin/embyrelay-agent ./cmd/agent
run:
	go run ./cmd/server
agent:
	go run ./cmd/agent
clean:
	rm -rf bin
