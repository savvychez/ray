export GOTOOLCHAIN ?= auto

.PHONY: all ray web test devstack clean

all: ray web

ray: ## build the server binary
	go build -o bin/ray ./cmd/ray

web: ## build the PWA into web/dist
	./web/build.sh

test:
	go test ./...

devstack: web ## local DERP + demo server + PWA, for browser testing
	go test ./cmd/ray -run TestDevStack -devstack -v -timeout 0

clean:
	rm -rf bin web/dist
