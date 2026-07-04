package app

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommandPrintsVersionAndGeneratedDate(t *testing.T) {
	withBuildInfo(t, "1.2.3", "abc123", "2026-07-04T12:34:56Z")

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "subkit 1.2.3\n") {
		t.Fatalf("version output missing version: %q", got)
	}
	if !strings.Contains(got, "generated July 4, 2026 at 12:34 UTC\n") {
		t.Fatalf("version output missing generated date: %q", got)
	}
	if strings.Contains(got, "abc123") || strings.Contains(got, "commit") {
		t.Fatalf("non-verbose output included commit: %q", got)
	}
}

func TestVersionCommandVerbosePrintsCommit(t *testing.T) {
	withBuildInfo(t, "1.2.3", "abc123", "2026-07-04T12:34:56Z")

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"version", "--verbose"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "commit abc123\n") {
		t.Fatalf("verbose output missing commit: %q", got)
	}
}

func TestVersionCommandFallsBackToRawBuildDate(t *testing.T) {
	withBuildInfo(t, "dev", "unknown", "not-rfc3339")

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "generated not-rfc3339\n") {
		t.Fatalf("version output missing raw date fallback: %q", got)
	}
}

func withBuildInfo(t *testing.T, nextVersion string, nextCommit string, nextDate string) {
	t.Helper()

	oldVersion := version
	oldCommit := commit
	oldDate := date
	version = nextVersion
	commit = nextCommit
	date = nextDate

	t.Cleanup(func() {
		version = oldVersion
		commit = oldCommit
		date = oldDate
	})
}
