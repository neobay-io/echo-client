package core

import (
	"strings"
	"testing"
)

func TestRedactArgs_FlagValue(t *testing.T) {
	args := []string{"--api-key", "sk-secret-123", "--verbose"}
	out := RedactArgs(args)

	if out[1] != "***" {
		t.Errorf("expected redacted, got %q", out[1])
	}
	if out[2] != "--verbose" {
		t.Errorf("non-sensitive arg modified: %q", out[2])
	}
}

func TestRedactArgs_EqualFormat(t *testing.T) {
	args := []string{"--api-key=sk-secret-123", "--verbose"}
	out := RedactArgs(args)

	if !strings.HasPrefix(out[0], "--api-key=") || !strings.HasSuffix(out[0], "***") {
		t.Errorf("expected --api-key=***, got %q", out[0])
	}
}

func TestRedactArgs_MultipleFlags(t *testing.T) {
	args := []string{"--token", "tok-123", "--secret", "s3cr3t", "--model", "gpt-4"}
	out := RedactArgs(args)

	if out[1] != "***" {
		t.Errorf("--token value not redacted: %q", out[1])
	}
	if out[3] != "***" {
		t.Errorf("--secret value not redacted: %q", out[3])
	}
	if out[5] != "gpt-4" {
		t.Errorf("--model value should not be redacted: %q", out[5])
	}
}

func TestRedactArgs_NoModifyOriginal(t *testing.T) {
	args := []string{"--api-key", "sk-secret"}
	_ = RedactArgs(args)
	if args[1] != "sk-secret" {
		t.Error("original args should not be modified")
	}
}

func TestRedactArgs_ShortFlag(t *testing.T) {
	args := []string{"-k", "my-key"}
	out := RedactArgs(args)
	if out[1] != "***" {
		t.Errorf("-k value not redacted: %q", out[1])
	}
}

func TestRedactArgs_Empty(t *testing.T) {
	out := RedactArgs(nil)
	if len(out) != 0 {
		t.Errorf("expected empty, got %v", out)
	}
}

// Review finding 13: prompts can hold pasted secrets; logs keep their length.
func TestRedactArgs_Prompts(t *testing.T) {
	out := RedactArgs([]string{"--output-format", "stream-json", "--prompt=my key is sk-123", "-p", "deploy with token abc", "-p", "--next"})
	joined := strings.Join(out, " ")
	if strings.Contains(joined, "sk-123") || strings.Contains(joined, "abc") {
		t.Fatalf("prompt leaked: %q", out)
	}
	if out[2] != "--prompt=<prompt: 16 chars>" || out[4] != "<prompt: 21 chars>" || out[6] != "--next" {
		t.Fatalf("out = %q", out)
	}
}
