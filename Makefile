# Developer shortcuts. `make check` runs what CI gates on, except the image build.

# Tools run with `go run` at pinned versions, as in CI; Dependabot does not
# bump these, so update them by hand together with .github/workflows/ci.yml.
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0
# `go run pkg@version` would otherwise pick the tool's own minimum Go,
# which cannot load this module.
export GOTOOLCHAIN := $(shell go env GOVERSION)

.PHONY: check fmt vet lint test vuln build compose hooks

check: fmt vet lint test vuln compose

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "$$out"; echo "gofmt: run gofmt -w on the files above"; exit 1; fi

vet:
	go vet ./...

lint:
	go run $(STATICCHECK) ./...

test:
	go test -race ./...

vuln:
	go run $(GOVULNCHECK) ./...

build:
	CGO_ENABLED=0 go build ./cmd/coachd

compose:
	docker compose --env-file deploy/.env.example -f deploy/compose.yaml config -q

# Pre-commit gofmt and a commit-msg DCO check, from .githooks/.
hooks:
	git config core.hooksPath .githooks
