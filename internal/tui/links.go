package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/diffsec/sibyl/internal/hostd"
)

// service is one web address that Sibyl serves through Caddy.
type service struct {
	name string
	url  string
	note string
}

// serviceLinks returns the addresses of the Sibyl services on each IPv4
// address of the connected interfaces. nftables redirects 443, 8443 and
// 9443 to Caddy.
func serviceLinks(st hostd.State) []service {
	var out []service
	for _, it := range st.Interfaces {
		if it.State != "connected" {
			continue
		}
		for _, a := range it.Addresses {
			ip, _, _ := strings.Cut(a, "/")
			out = append(out,
				service{"Open WebUI", "https://" + ip + "/", "chat"},
				service{"Arcane", "https://" + ip + ":9443/", "containers"},
				service{"OpenAI API", "https://" + ip + ":8443/v1", "needs a client key (Router screen)"},
				service{"Ollama API", "https://" + ip + ":8443/api", "needs a client key (Router screen)"},
			)
		}
	}
	return out
}

// link renders text as a terminal hyperlink (OSC 8). Terminals without
// hyperlink support show the plain text.
func link(url string) string {
	return lipgloss.NewStyle().Hyperlink(url).Underline(true).Render(url)
}

func renderLinks(st hostd.State) string {
	links := serviceLinks(st)
	if len(links) == 0 {
		return "  " + mutedSty.Render("no network address yet") + "\n"
	}
	var b strings.Builder
	for _, s := range links {
		fmt.Fprintf(&b, "  %-11s %s  %s\n", s.name, link(s.url), mutedSty.Render(s.note))
	}
	b.WriteString("  " + mutedSty.Render("The certificate is self-signed: the browser warns once. Ctrl or Cmd+click opens a link.") + "\n")
	return b.String()
}
