package hostd

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Tailscale joins the host to a tailnet. Every port of the host is open on
// the tailscale0 interface (see /etc/nftables/sibyl.nft), so the tailnet's
// access rules decide who reaches Open WebUI, the APIs, Arcane and the
// containers that Arcane runs.
const (
	// TailscaleUp starts tailscaled and logs in, with an auth key or with a
	// login link that the state then shows.
	TailscaleUp Action = "tailscale-up"
	// TailscaleDown logs out and stops tailscaled.
	TailscaleDown Action = "tailscale-down"
)

// TailscaleUnit is the Tailscale daemon.
const TailscaleUnit = "tailscaled.service"

// TailscaleUpRequest is the body of a tailscale-up request. AuthKeyRef
// names a file in the secret directory, <ref>.key, that holds an auth key.
// The key itself never goes into the request queue, which is on disk: the
// secret directory is in memory, under /run. Empty logs in with a link.
type TailscaleUpRequest struct {
	AuthKeyRef string `json:"auth_key_ref,omitempty"`
}

var (
	keyRefRE = regexp.MustCompile(`^[0-9]{1,20}$`)
	// AuthKeyRE matches a Tailscale auth key, such as tskey-auth-k123-abc.
	AuthKeyRE = regexp.MustCompile(`^tskey-[A-Za-z0-9-]{8,200}$`)
)

// Validate checks a tailscale-up request.
func (t TailscaleUpRequest) Validate() error {
	if t.AuthKeyRef != "" && !keyRefRE.MatchString(t.AuthKeyRef) {
		return fmt.Errorf("auth_key_ref %q: use the digits of the key file name", t.AuthKeyRef)
	}
	return nil
}

// TailscaleState is the Tailscale part of the host state.
type TailscaleState struct {
	// Enabled is true when tailscaled starts at boot.
	Enabled bool `json:"enabled"`
	// Backend is tailscaled's state: NeedsLogin, Starting, Running,
	// Stopped, or empty when tailscaled does not run.
	Backend string `json:"backend,omitempty"`
	// AuthURL is the login link while the host waits for a login.
	AuthURL  string   `json:"auth_url,omitempty"`
	HostName string   `json:"host_name,omitempty"`
	DNSName  string   `json:"dns_name,omitempty"`
	Tailnet  string   `json:"tailnet,omitempty"`
	IPs      []string `json:"ips,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// tailscaleStatus is the part of `tailscale status --json` that Sibyl reads.
type tailscaleStatus struct {
	BackendState   string
	AuthURL        string
	Self           *struct{ HostName, DNSName string }
	TailscaleIPs   []string
	CurrentTailnet *struct{ Name string }
}

func parseTailscaleStatus(b []byte) (TailscaleState, error) {
	var s tailscaleStatus
	if err := json.Unmarshal(b, &s); err != nil {
		return TailscaleState{}, fmt.Errorf("tailscale status: %w", err)
	}
	st := TailscaleState{Backend: s.BackendState, IPs: s.TailscaleIPs}
	if s.BackendState == "NeedsLogin" {
		st.AuthURL = s.AuthURL
	}
	if s.Self != nil && s.BackendState == "Running" {
		st.HostName, st.DNSName = s.Self.HostName, strings.TrimSuffix(s.Self.DNSName, ".")
	}
	if s.CurrentTailnet != nil {
		st.Tailnet = s.CurrentTailnet.Name
	}
	return st, nil
}

// tailscaleState reads the Tailscale state, or nil when the image has no
// tailscaled.
func (a *Agent) tailscaleState(ctx context.Context) *TailscaleState {
	// is-enabled exits 1 for a disabled unit, and prints "disabled". It
	// prints nothing for a unit that does not exist.
	out, _ := a.Run(ctx, "", "systemctl", "is-enabled", "--", TailscaleUnit)
	enabled := strings.TrimSpace(out)
	if enabled == "" {
		return nil
	}
	active, _ := a.Run(ctx, "", "systemctl", "is-active", "--", TailscaleUnit)
	if strings.TrimSpace(active) != "active" {
		return &TailscaleState{Enabled: enabled == "enabled"}
	}
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, err := a.Run(sctx, "", "tailscale", "status", "--json")
	if err != nil {
		return &TailscaleState{Enabled: enabled == "enabled", Error: err.Error()}
	}
	st, err := parseTailscaleStatus([]byte(b))
	if err != nil {
		st.Error = err.Error()
	}
	st.Enabled = enabled == "enabled"
	return &st
}

// tailscaleUp starts tailscaled and logs in. With no auth key it returns
// the login link, and the host joins when someone opens the link.
func (a *Agent) tailscaleUp(ctx context.Context, req TailscaleUpRequest) (string, error) {
	var key string
	if req.AuthKeyRef != "" {
		b, err := readSecret(a.SecretDir, req.AuthKeyRef+".key")
		if err != nil {
			return "", fmt.Errorf("auth key: %w", err)
		}
		key = strings.TrimSpace(string(b))
		if !AuthKeyRE.MatchString(key) {
			return "", errors.New("auth key: it must start with tskey-")
		}
	}
	if _, err := a.Run(ctx, "", "systemctl", "enable", "--now", "--", TailscaleUnit); err != nil {
		return "", err
	}
	// --reset makes every login use the same settings. The host keeps its
	// own DNS, so the model downloads do not depend on the tailnet.
	args := []string{"up", "--reset", "--accept-dns=false"}
	if key != "" {
		// The key goes to tailscale in a file in sibyl-hostd's private /tmp,
		// so it is not in the process list.
		f, err := os.CreateTemp("", "ts-key-*")
		if err != nil {
			return "", err
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := f.WriteString(key); err != nil {
			_ = f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		args = append(args, "--auth-key=file:"+f.Name(), "--timeout=60s")
	} else {
		// With no key, up waits for the login. It stops after a short time,
		// and the link stays valid in the state.
		args = append(args, "--timeout=10s")
	}
	_, upErr := a.Run(ctx, "", "tailscale", args...)
	st := a.tailscaleState(ctx)
	switch {
	case st != nil && st.Backend == "Running":
		return fmt.Sprintf("on the tailnet %s as %s, %s", st.Tailnet, st.DNSName, strings.Join(st.IPs, " ")), nil
	case st != nil && st.AuthURL != "":
		return "open " + st.AuthURL + " to add this host to your tailnet", nil
	case upErr != nil:
		return "", upErr
	}
	return "tailscale started", nil
}

// tailscaleDown logs out and stops tailscaled.
func (a *Agent) tailscaleDown(ctx context.Context) (string, error) {
	// A host that never logged in has nothing to log out of.
	_, _ = a.Run(ctx, "", "tailscale", "logout")
	if _, err := a.Run(ctx, "", "systemctl", "disable", "--now", "--", TailscaleUnit); err != nil {
		return "", err
	}
	return "left the tailnet; tailscaled is off", nil
}

// TLSService reissues the server certificate. It adds every private IPv4
// address of the host, the Tailscale address too.
const TLSService = "sibyl-tls.service"

// certCovers tells whether the certificate at path holds every address.
func certCovers(path string, ips []string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return false
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return false
	}
	for _, s := range ips {
		ip, err := netip.ParseAddr(s)
		if err != nil || !ip.Is4() {
			continue
		}
		if !slices.ContainsFunc(c.IPAddresses, func(x net.IP) bool {
			a, ok := netip.AddrFromSlice(x)
			return ok && a.Unmap() == ip
		}) {
			return false
		}
	}
	return true
}

// tailscaleCert starts a reissue of the server certificate when the
// Tailscale address is not in it, so https://<tailscale address> works
// soon after a login.
func (a *Agent) tailscaleCert(ctx context.Context, st *TailscaleState) {
	if st == nil || st.Backend != "Running" || len(st.IPs) == 0 || a.TLSCert == "" {
		return
	}
	if certCovers(a.TLSCert, st.IPs) {
		return
	}
	if _, err := a.Run(ctx, "", "systemctl", "start", "--no-block", "--", TLSService); err != nil {
		a.Log.Warn("reissue the certificate for the tailscale address", "err", err)
	}
}
