package core

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"testing"
)

func TestReleaseAssetNameUsesRequestedPrefix(t *testing.T) {
	got := releaseAssetName(currentBinaryPrefix, "v1.2.3", "linux", "amd64")
	if want := "echo-client-v1.2.3-linux-amd64.tar.gz"; got != want {
		t.Fatalf("releaseAssetName(current) = %q, want %q", got, want)
	}
	got = releaseAssetName(legacyBinaryPrefix, "v1.2.3", "windows", "arm64")
	if want := "cc-connect-v1.2.3-windows-arm64.zip"; got != want {
		t.Fatalf("releaseAssetName(legacy) = %q, want %q", got, want)
	}
}

func TestExtractBinaryFromTarGzAcceptsLegacyAndCurrentPrefixes(t *testing.T) {
	for _, name := range []string{"echo-client-v1.2.3-linux-amd64", "cc-connect-v1.2.3-linux-amd64"} {
		data := tarGzArchive(t, name, []byte("payload:"+name))
		got, err := extractBinaryFromTarGz(data)
		if err != nil {
			t.Fatalf("extractBinaryFromTarGz(%s): %v", name, err)
		}
		if want := "payload:" + name; string(got) != want {
			t.Fatalf("extractBinaryFromTarGz(%s) = %q, want %q", name, string(got), want)
		}
	}
}

func TestExtractBinaryFromZipAcceptsLegacyAndCurrentPrefixes(t *testing.T) {
	for _, name := range []string{"echo-client-v1.2.3-windows-amd64.exe", "cc-connect-v1.2.3-windows-amd64.exe"} {
		data := zipArchive(t, name, []byte("payload:"+name))
		got, err := extractBinaryFromZip(data)
		if err != nil {
			t.Fatalf("extractBinaryFromZip(%s): %v", name, err)
		}
		if want := "payload:" + name; string(got) != want {
			t.Fatalf("extractBinaryFromZip(%s) = %q, want %q", name, string(got), want)
		}
	}
}

func tarGzArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("Close gzip: %v", err)
	}
	return buf.Bytes()
}

func zipArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatalf("Create zip entry: %v", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(body)); err != nil {
		t.Fatalf("Write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("Close zip: %v", err)
	}
	return buf.Bytes()
}

func TestMatchesReleaseBinaryName(t *testing.T) {
	cases := map[string]bool{
		"echo-client-v1.0.0-linux-amd64":           true,
		"cc-connect-v1.0.0-linux-amd64":            true,
		"other-v1.0.0-linux-amd64":                 false,
		fmt.Sprintf("%s.exe", currentBinaryPrefix): true,
	}
	for name, want := range cases {
		if got := matchesReleaseBinaryName(name); got != want {
			t.Fatalf("matchesReleaseBinaryName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSemverCompare(t *testing.T) {
	tests := []struct {
		a, b string
		want int // >0, <0, or 0
	}{
		{"v1.0.0", "v1.0.0", 0},
		{"v1.0.1", "v1.0.0", 1},
		{"v1.0.0", "v1.0.1", -1},
		{"v2.0.0", "v1.9.9", 1},
		{"v1.1.0", "v1.0.9", 1},

		// pre-release vs release
		{"v1.0.0", "v1.0.0-beta.1", 1},
		{"v1.0.0-beta.1", "v1.0.0", -1},

		// pre-release ordering
		{"v1.0.0-beta.2", "v1.0.0-beta.1", 1},
		{"v1.0.0-beta.1", "v1.0.0-beta.2", -1},
		{"v1.0.0-beta.1", "v1.0.0-beta.1", 0},

		// different pre-release prefixes
		{"v1.0.0-rc.1", "v1.0.0-beta.1", 1},
		{"v1.0.0-alpha.1", "v1.0.0-beta.1", -1},

		// without 'v' prefix
		{"1.0.0", "1.0.0", 0},
		{"1.0.1", "1.0.0", 1},
	}

	for _, tt := range tests {
		got := semverCompare(tt.a, tt.b)
		if (tt.want > 0 && got <= 0) || (tt.want < 0 && got >= 0) || (tt.want == 0 && got != 0) {
			t.Errorf("semverCompare(%q, %q) = %d, want sign %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	s := parseSemver("v1.2.3-beta.4")
	if s.major != 1 || s.minor != 2 || s.patch != 3 {
		t.Errorf("parsed %+v, want 1.2.3", s)
	}
	if s.pre != "beta.4" {
		t.Errorf("pre = %q, want beta.4", s.pre)
	}
	if s.preNum != 4 {
		t.Errorf("preNum = %d, want 4", s.preNum)
	}
}

func TestParseSemver_NoPreRelease(t *testing.T) {
	s := parseSemver("v2.0.0")
	if s.major != 2 || s.minor != 0 || s.patch != 0 {
		t.Errorf("parsed %+v, want 2.0.0", s)
	}
	if s.pre != "" {
		t.Errorf("pre = %q, want empty", s.pre)
	}
}

func TestParseSemver_Invalid(t *testing.T) {
	s := parseSemver("not-a-version")
	if s.major != 0 && s.minor != 0 && s.patch != 0 {
		t.Errorf("expected zero semver for invalid input, got %+v", s)
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"1.0.0", "v1.0.0"},
		{"v1.0.0", "v1.0.0"},
		{" v1.0.0 ", "v1.0.0"},
		{"  2.3.4", "v2.3.4"},
	}
	for _, tt := range tests {
		got := normalizeVersion(tt.in)
		if got != tt.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
