package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestPrintUsageUsesEchoClientName(t *testing.T) {
	version = "v1.2.3"
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	printUsage()

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, "echo-client [flags]") {
		t.Fatalf("usage missing echo-client command name:\n%s", text)
	}
	if strings.Contains(text, "cc-connect [flags]") {
		t.Fatalf("usage still contains legacy command name:\n%s", text)
	}
}
