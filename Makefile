.PHONY: build install test clean hooks install-skill install-agents install-integration

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

# Installs SKILL.md as a personal skill for every tool that reads the shared
# SKILL.md standard (folder-per-skill: <skills-dir>/agent-tmux/SKILL.md).
install-skill:
	mkdir -p $(HOME)/.claude/skills/agent-tmux
	ln -sf $(CURDIR)/SKILL.md $(HOME)/.claude/skills/agent-tmux/SKILL.md
	mkdir -p $(HOME)/.copilot/skills/agent-tmux
	ln -sf $(CURDIR)/SKILL.md $(HOME)/.copilot/skills/agent-tmux/SKILL.md
	@echo "Installed SKILL.md for Claude Code (~/.claude/skills/agent-tmux) and"
	@echo "Copilot CLI (~/.copilot/skills/agent-tmux)."
	@echo "VS Code Copilot's skill discovery path is still settling upstream — if it"
	@echo "doesn't pick this up automatically, check VS Code's Skills UI/settings for"
	@echo "the current personal-skills directory."

# Installs the ready-made 'investigator' custom agent for every supported tool.
install-agents:
	mkdir -p $(HOME)/.config/Code/User/prompts
	ln -sf $(CURDIR)/agents/investigator.vscode.agent.md $(HOME)/.config/Code/User/prompts/investigator.agent.md
	mkdir -p $(HOME)/.copilot/agents
	ln -sf $(CURDIR)/agents/investigator.copilot-cli.agent.md $(HOME)/.copilot/agents/investigator.agent.md
	mkdir -p $(HOME)/.claude/agents
	ln -sf $(CURDIR)/agents/investigator.claude.md $(HOME)/.claude/agents/investigator.md
	@echo "Installed the 'investigator' agent for VS Code Copilot, Copilot CLI, and Claude Code."

install-integration: install-skill install-agents
