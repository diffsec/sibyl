package hostd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tsRunning = `{"BackendState":"Running","AuthURL":"","TailscaleIPs":["100.101.102.103","fd7a:115c:a1e0::1"],
"Self":{"HostName":"sibyl","DNSName":"sibyl.tail1234.ts.net."},"CurrentTailnet":{"Name":"ben@example.com"}}`

const tsNeedsLogin = `{"BackendState":"NeedsLogin","AuthURL":"https://login.tailscale.com/a/abc123","TailscaleIPs":null,
"Self":{"HostName":"x","DNSName":""},"CurrentTailnet":null}`

func TestTailscaleDecodeRefusals(t *testing.T) {
	for _, bad := range []string{
		`{"auth_key_ref":"../../etc/shadow"}`,
		`{"auth_key_ref":"1/2"}`,
		`{"auth_key_ref":"abc"}`,
		`{"auth_key_ref":"123456789012345678901"}`,
		`{"auth_key":"tskey-auth-abcdefgh"}`, // the key itself never goes in the queue
		`{"flags":["--ssh"]}`,
	} {
		if _, err := Decode(TailscaleUp, []byte(bad)); err == nil {
			t.Errorf("%s passed", bad)
		}
	}
	if _, err := Decode(TailscaleDown, []byte(`{"all":true}`)); err == nil {
		t.Error("tailscale-down took a field")
	}
	for _, ok := range []string{`{}`, `{"auth_key_ref":"1793334727815241160"}`} {
		if _, err := Decode(TailscaleUp, []byte(ok)); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
}

func tsAgent(t *testing.T) *testAgent {
	ta := newAgent(t)
	ta.SecretDir = t.TempDir()
	ta.host.answers["systemctl is-enabled -- "+TailscaleUnit] = "enabled\n"
	ta.host.answers["systemctl is-active -- "+TailscaleUnit] = "active\n"
	return ta
}

func TestTailscaleUpWithKey(t *testing.T) {
	ta := tsAgent(t)
	ta.host.answers["tailscale status --json"] = tsRunning
	key := "tskey-auth-kAbC123-secretpart"
	if err := os.WriteFile(filepath.Join(ta.SecretDir, "42.key"), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// tailscale reads the key from the file that its argument names.
	var got string
	run := ta.Run
	ta.Run = func(ctx context.Context, stdin, name string, args ...string) (string, error) {
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "--auth-key=file:"); ok {
				b, _ := os.ReadFile(p)
				got = string(b)
			}
		}
		return run(ctx, stdin, name, args...)
	}
	r := ta.submit(t, "1", TailscaleUp, `{"auth_key_ref":"42"}`)
	if !r.OK || !strings.Contains(r.Message, "sibyl.tail1234.ts.net") || !strings.Contains(r.Message, "100.101.102.103") {
		t.Fatalf("result %+v", r)
	}
	if got != key {
		t.Errorf("tailscale read %q from the key file", got)
	}
	for _, c := range ta.host.calls {
		if strings.Contains(c, "tskey-") {
			t.Errorf("the key is in a command line: %s", c)
		}
	}
	if !ta.host.ran("systemctl enable --now -- "+TailscaleUnit) || !ta.host.ran("tailscale up --reset --accept-dns=false --auth-key=file:") {
		t.Errorf("calls %v", ta.host.calls)
	}
	if left, _ := os.ReadDir(ta.SecretDir); len(left) != 0 {
		t.Errorf("the key file stayed: %v", left)
	}
	st := ta.state(t).Tailscale
	if st == nil || !st.Enabled || st.Backend != "Running" || st.DNSName != "sibyl.tail1234.ts.net" || st.Tailnet != "ben@example.com" {
		t.Errorf("state %+v", st)
	}
}

func TestTailscaleUpRefusesBadKey(t *testing.T) {
	ta := tsAgent(t)
	if err := os.WriteFile(filepath.Join(ta.SecretDir, "7.key"), []byte("--ssh"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := ta.submit(t, "1", TailscaleUp, `{"auth_key_ref":"7"}`); r.OK || ta.host.ran("tailscale up") {
		t.Errorf("a bad key ran: %+v %v", r, ta.host.calls)
	}
	if r := ta.submit(t, "2", TailscaleUp, `{"auth_key_ref":"8"}`); r.OK || ta.host.ran("systemctl enable") {
		t.Errorf("a missing key file ran: %+v %v", r, ta.host.calls)
	}
}

func TestTailscaleUpWithLink(t *testing.T) {
	ta := tsAgent(t)
	ta.host.answers["tailscale status --json"] = tsNeedsLogin
	r := ta.submit(t, "1", TailscaleUp, `{}`)
	if !r.OK || !strings.Contains(r.Message, "https://login.tailscale.com/a/abc123") {
		t.Fatalf("result %+v", r)
	}
	if !ta.host.ran("tailscale up --reset --accept-dns=false --timeout=10s") {
		t.Errorf("calls %v", ta.host.calls)
	}
	if st := ta.state(t).Tailscale; st == nil || st.AuthURL != "https://login.tailscale.com/a/abc123" || len(st.IPs) != 0 {
		t.Errorf("state %+v", st)
	}
}

func TestTailscaleDownAndAbsent(t *testing.T) {
	ta := tsAgent(t)
	if r := ta.submit(t, "1", TailscaleDown, `{}`); !r.OK || !ta.host.ran("tailscale logout") ||
		!ta.host.ran("systemctl disable --now -- "+TailscaleUnit) {
		t.Errorf("down %+v %v", r, ta.host.calls)
	}
	// An image without tailscaled has no Tailscale state.
	ta = newAgent(t)
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ta.state(t).Tailscale != nil {
		t.Error("state without tailscaled")
	}
}

func TestTailscaleCertReissue(t *testing.T) {
	ta := tsAgent(t)
	ta.host.answers["tailscale status --json"] = tsRunning
	ta.TLSCert = filepath.Join(t.TempDir(), "server.crt")
	writeCert(t, ta.TLSCert, "192.168.1.26")
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !ta.host.ran("systemctl start --no-block -- " + TLSService) {
		t.Errorf("no reissue for a new address: %v", ta.host.calls)
	}
	ta.host.calls = nil
	writeCert(t, ta.TLSCert, "192.168.1.26", "100.101.102.103")
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ta.host.ran("systemctl start --no-block -- " + TLSService) {
		t.Error("a reissue for a certificate that has the address")
	}
}

func writeCert(t *testing.T, path string, ips ...string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour)}
	for _, s := range ips {
		tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP(s))
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}
