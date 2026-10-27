// Command sibyl-tui is the Sibyl console interface.
//
// It needs the System API address and the API key:
//
//	SIBYL_API_URL       default http://system-api:3001 (inside the sibyl network)
//	SIBYL_API_KEY_FILE  default /run/secrets/api-key
//	SIBYL_CA_FILE       optional PEM file to trust, for the host's self-signed certificate
//	SIBYL_TLS_SERVER_NAME  optional name to verify, such as sibyl.local when connecting to 127.0.0.1
//	SIBYL_WIZARD        1 to walk through the setup steps (sibyl setup)
package main

import (
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/ihavespoons/sibyl/internal/client"
	"github.com/ihavespoons/sibyl/internal/tui"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "sibyl-tui:", err)
		os.Exit(1)
	}
}

func run() error {
	key, err := os.ReadFile(env("SIBYL_API_KEY_FILE", "/run/secrets/api-key"))
	if err != nil {
		return err
	}
	c, err := client.New(env("SIBYL_API_URL", "http://system-api:3001"), strings.TrimSpace(string(key)), os.Getenv("SIBYL_CA_FILE"), os.Getenv("SIBYL_TLS_SERVER_NAME"))
	if err != nil {
		return err
	}
	opts := tui.Options{Wizard: os.Getenv("SIBYL_WIZARD") == "1", Fingerprint: fingerprint(os.Getenv("SIBYL_CA_FILE"))}
	_, err = tea.NewProgram(tui.New(c, opts)).Run()
	return err
}

// fingerprint returns the SHA-256 fingerprint of the first certificate in a
// PEM file, as colon-separated hex, or "unknown".
func fingerprint(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return "unknown"
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return "unknown"
	}
	sum := sha256.Sum256(block.Bytes)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}
