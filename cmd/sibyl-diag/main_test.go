package main

import (
	"flag"
	"testing"
)

func TestModeAndFlagsTakesTheFlagOnEitherSide(t *testing.T) {
	for _, args := range [][]string{
		{"manual", "-endpoint", "https://ingest.example"},
		{"-endpoint", "https://ingest.example", "manual"},
	} {
		fs := flag.NewFlagSet("consent", flag.ContinueOnError)
		endpoint := fs.String("endpoint", "", "")
		mode, err := modeAndFlags(fs, args)
		if err != nil || mode != "manual" || *endpoint != "https://ingest.example" {
			t.Errorf("%v: mode %q, endpoint %q, err %v", args, mode, *endpoint, err)
		}
	}
	fs := flag.NewFlagSet("consent", flag.ContinueOnError)
	fs.String("endpoint", "", "")
	if _, err := modeAndFlags(fs, []string{"manual", "-endpoint", "x", "extra"}); err == nil {
		t.Error("an extra argument was accepted")
	}
}
