package main

import (
	"bytes"
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
)

// `skill` must print the repo's SKILL.md byte for byte, with the frontmatter
// skill tools need: they install it as <skills-dir>/<name>/SKILL.md.
func TestSkillPrintsSkillMD(t *testing.T) {
	want, err := os.ReadFile("../../SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"skill"}, &out, &errOut); code != 0 {
		t.Fatalf("skill: exit %d, stderr %q", code, errOut.String())
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("skill output differs from SKILL.md")
	}

	front, _, ok := strings.Cut(strings.TrimPrefix(out.String(), "---\n"), "\n---\n")
	if !ok || !strings.HasPrefix(out.String(), "---\n") {
		t.Fatal("SKILL.md has no frontmatter")
	}
	if !regexp.MustCompile(`(?m)^name: agent-tmux$`).MatchString(front) {
		t.Error("frontmatter lacks name: agent-tmux (the skill folder is named after it)")
	}
	if !regexp.MustCompile(`(?m)^description: \S`).MatchString(front) {
		t.Error("frontmatter lacks a description")
	}
}

// SKILL.md is installed alone, without the rest of this repo, so it must not
// send the agent to other files here.
func TestSkillIsSelfContained(t *testing.T) {
	var out, errOut bytes.Buffer
	run(context.Background(), []string{"skill"}, &out, &errOut)
	if refs := regexp.MustCompile(`[\w./-]+\.md\b`).FindAllString(out.String(), -1); len(refs) > 0 {
		t.Errorf("SKILL.md points at repo files that are not installed with it: %v", refs)
	}
}
