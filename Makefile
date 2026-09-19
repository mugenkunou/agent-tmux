.PHONY: build install test clean hooks

GOTMPDIR := $(CURDIR)/.tmp/go

build:
	mkdir -p bin $(GOTMPDIR)
	GOTMPDIR=$(GOTMPDIR) go build -o bin/agent-tmux ./cmd/agent-tmux

install:
	mkdir -p $(GOTMPDIR)
	GOTMPDIR=$(GOTMPDIR) go install ./cmd/agent-tmux

test:
	mkdir -p $(GOTMPDIR)
	GOTMPDIR=$(GOTMPDIR) go test ./...

clean:
	rm -rf bin .tmp

hooks:
	git config core.hooksPath .githooks
	@echo "pre-commit secret scan enabled (requires gitleaks on PATH: https://github.com/gitleaks/gitleaks#installing)"
