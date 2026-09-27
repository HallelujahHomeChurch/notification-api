package main

import (
	"strings"
	"testing"
)

func TestParseArgsRequiresBoundedUTCWindow(t *testing.T) {
	if _, _, err := parseArgs([]string{"-from", "2026-09-27T02:00:00Z", "-to", "2026-09-27T03:00:00Z"}); err != nil {
		t.Fatalf("parseArgs(valid) = %v", err)
	}
	for _, args := range [][]string{
		{"-from", "2026-09-27T03:00:00Z", "-to", "2026-09-27T02:00:00Z"},
		{"-from", "2026-09-27T02:00:00Z", "-to", "2026-10-30T02:00:00Z"},
	} {
		if _, _, err := parseArgs(args); err == nil {
			t.Fatalf("parseArgs(%v) error = nil", args)
		}
	}
}

func TestReadEmailNormalizesWithoutEchoingInput(t *testing.T) {
	email, err := readEmail(strings.NewReader(" User@Example.Test \n"))
	if err != nil || email != "user@example.test" {
		t.Fatalf("readEmail() = %q, %v", email, err)
	}
	if _, err := readEmail(strings.NewReader("not-an-email\n")); err == nil {
		t.Fatal("readEmail() accepted invalid address")
	}
}
