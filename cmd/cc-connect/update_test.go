package main

import (
	"fmt"
	"runtime"
	"testing"
)

func TestBinaryAssetNameUsesEchoClientPrefix(t *testing.T) {
	tag := "v1.2.3"
	want := fmt.Sprintf("%s-%s-%s-%s", appName, tag, runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		want += ".exe"
	}
	if got := binaryAssetName(tag); got != want {
		t.Fatalf("binaryAssetName(%q) = %q, want %q", tag, got, want)
	}
}
