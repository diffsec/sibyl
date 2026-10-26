// Command sibyl-tui is the Sibyl console interface.
//
// It needs the System API address and the API key:
//
//	SIBYL_API_URL       default http://system-api:3001 (inside the sibyl network)
//	SIBYL_API_KEY_FILE  default /run/secrets/api-key
//	SIBYL_CA_FILE       optional PEM file to trust, for the host's self-signed certificate
//	SIBYL_TLS_SERVER_NAME  optional name to verify, such as sibyl.local when connecting to 127.0.0.1
package main

import (
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
	_, err = tea.NewProgram(tui.New(c)).Run()
	return err
}
