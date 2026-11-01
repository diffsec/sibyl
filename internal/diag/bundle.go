package diag

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// A bundle is a redacted tar.gz of one incident, with a manifest. Every
// bundle gets a diagnostic ID such as SBL-7K2M-9QXD. The owner quotes the ID
// to support with or without an upload, so a bundle copied off the box on a
// USB drive works the same way. Nothing is sent unless consent is "manual"
// (the owner runs `sibyl-diag submit`) or "automatic".

// idAlphabet is Crockford base32: no I, L, O or U, so an ID read over the
// phone is clear.
const idAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// checkChar uses odd weights. They are coprime with 32, so any single wrong
// character changes the check character.
func checkChar(body string) byte {
	total := 0
	for i := range len(body) {
		total += (2*i + 1) * strings.IndexByte(idAlphabet, body[i])
	}
	return idAlphabet[total%32]
}

// NewID returns a fresh diagnostic ID: 7 random characters and a check character.
func NewID() string {
	b := make([]byte, 7)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	body := make([]byte, 7)
	for i, v := range b {
		body[i] = idAlphabet[v%32]
	}
	full := string(body) + string(checkChar(string(body)))
	return "SBL-" + full[:4] + "-" + full[4:]
}

// ValidID checks the form and the check character of a diagnostic ID.
func ValidID(id string) bool {
	raw := strings.ReplaceAll(strings.TrimPrefix(strings.ToUpper(id), "SBL-"), "-", "")
	if len(raw) != 8 {
		return false
	}
	for i := range len(raw) {
		if strings.IndexByte(idAlphabet, raw[i]) < 0 {
			return false
		}
	}
	return checkChar(raw[:7]) == raw[7]
}

// ManifestFile is one file in a bundle.
type ManifestFile struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Bundle is a built bundle.
type Bundle struct {
	ID    string         `json:"diagnostic_id"`
	Path  string         `json:"path"`
	Files []ManifestFile `json:"files"`
}

// BuildBundle redacts an incident directory into BundleDir/<id>.tar.gz.
func (d *Diag) BuildBundle(incidentDir, note string) (Bundle, error) {
	host, _ := os.Hostname()
	r := Redactor{Salt: d.redactionSalt(), Hostname: host}
	id := NewID()
	if err := os.MkdirAll(d.BundleDir(), 0o750); err != nil {
		return Bundle{}, err
	}
	b := Bundle{ID: id, Path: filepath.Join(d.BundleDir(), id+".tar.gz")}

	var files []string
	err := filepath.WalkDir(incidentDir, func(p string, e os.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		return Bundle{}, err
	}
	sort.Strings(files)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, data []byte) error {
		hdr := &tar.Header{Name: id + "/" + name, Mode: 0o644, Size: int64(len(data)), ModTime: d.Now()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	budget := d.Cfg.Upload.MaxBundleBytes
	for _, p := range files {
		rel, err := filepath.Rel(incidentDir, p)
		if err != nil {
			return Bundle{}, err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return Bundle{}, err
		}
		// Every capture file is text. A file that is not cannot be redacted, so it stays out.
		if bytes.IndexByte(raw, 0) >= 0 {
			continue
		}
		data := []byte(r.Text(string(raw)))
		if int64(len(data)) > budget {
			data = append([]byte("<truncated, kept the tail>\n"), data[int64(len(data))-max(budget, 0):]...)
		}
		budget -= int64(len(data))
		name := "incident/" + filepath.ToSlash(rel)
		if err := add(name, data); err != nil {
			return Bundle{}, err
		}
		sum := sha256.Sum256(data)
		b.Files = append(b.Files, ManifestFile{Path: name, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])})
	}
	manifest, err := json.MarshalIndent(map[string]any{
		"diagnostic_id": id, "device_id": d.DeviceID(), "created": d.Now().UTC(),
		"version": d.readShare("version"), "gpu_variant": d.readShare("gpu-variant"),
		"incident": filepath.Base(incidentDir), "note": r.Text(note), "redacted": true, "files": b.Files,
	}, "", "  ")
	if err != nil {
		return Bundle{}, err
	}
	if err := add("manifest.json", manifest); err != nil {
		return Bundle{}, err
	}
	if err := tw.Close(); err != nil {
		return Bundle{}, err
	}
	if err := gz.Close(); err != nil {
		return Bundle{}, err
	}
	if err := writeAtomic(b.Path, buf.Bytes(), 0o640, "", -1); err != nil {
		return Bundle{}, err
	}
	d.PruneBundles()
	return b, nil
}

// PruneBundles removes the oldest bundles, with their upload markers, past
// the incident limits. A queued bundle that goes is not uploaded.
func (d *Diag) PruneBundles() {
	paths, _ := filepath.Glob(filepath.Join(d.BundleDir(), "SBL-*.tar.gz"))
	type bundle struct {
		path string
		size int64
		mod  time.Time
	}
	var all []bundle
	var total int64
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			all = append(all, bundle{p, info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.Before(all[j].mod) })
	c := d.Cfg.Capture
	for i := 0; i < len(all) && (len(all)-i > c.MaxIncidents || total > c.MaxIncidentBytes); i++ {
		for _, p := range []string{all[i].path, all[i].path + ".sent", all[i].path + ".pending"} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				d.Log.Warn("prune bundle", "err", err)
			}
		}
		total -= all[i].size
	}
}

// uploadClient refuses redirects, so a redirect cannot send a bundle to an
// address that CheckEndpoint did not check.
var uploadClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}}

// CheckEndpoint requires HTTPS, or plain HTTP to loopback for tests.
func CheckEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid upload endpoint %q", endpoint)
	}
	loopback := u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return fmt.Errorf("the upload endpoint must use https: %s", endpoint)
	}
	return nil
}

// ErrConsentOff is returned when an upload is attempted with consent off.
var ErrConsentOff = errors.New("upload consent is off; turn it on with `sibyl-diag consent manual`")

// Upload sends a bundle to {endpoint}/v1/bundles and returns the diagnostic
// ID that the service stored.
func (d *Diag) Upload(ctx context.Context, path string) (string, error) {
	if d.LoadConsent().Mode == ConsentOff {
		return "", ErrConsentOff
	}
	endpoint := d.Endpoint()
	if endpoint == "" {
		return "", errors.New("no upload endpoint is configured; set one with `sibyl-diag consent manual -endpoint https://...`")
	}
	if err := CheckEndpoint(endpoint); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if limit := d.Cfg.Upload.MaxBundleBytes; int64(len(data)) > limit {
		return "", fmt.Errorf("the bundle is %d bytes, over max_bundle_bytes (%d)", len(data), limit)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".tar.gz")
	sum := sha256.Sum256(data)
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/v1/bundles",
		bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("X-Sibyl-Diagnostic-Id", id)
	req.Header.Set("X-Sibyl-Device-Id", d.DeviceID())
	req.Header.Set("X-Sibyl-Version", d.readShare("version"))
	req.Header.Set("X-Content-Sha256", hex.EncodeToString(sum[:]))
	resp, err := uploadClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("the server refused the bundle: HTTP %d", resp.StatusCode)
	}
	var v struct {
		ID string `json:"diagnostic_id"`
	}
	if json.Unmarshal(body, &v) != nil || v.ID == "" {
		v.ID = id
	}
	if err := writeAtomic(path+".sent", append(body, '\n'), 0o640, "", -1); err != nil {
		return v.ID, err
	}
	if err := os.Remove(path + ".pending"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return v.ID, err
	}
	return v.ID, nil
}

// MarkPending queues a bundle for UploadPending.
func (d *Diag) MarkPending(path string) {
	if err := os.WriteFile(path+".pending", nil, 0o640); err != nil {
		d.Log.Error("queue bundle", "err", err)
	}
}

// UploadPending sends the queued bundles in automatic mode, oldest first.
// It stops at the first failure and tries again on the next run.
func (d *Diag) UploadPending(ctx context.Context) ([]string, error) {
	if d.LoadConsent().Mode != ConsentAutomatic {
		return nil, nil
	}
	markers, _ := filepath.Glob(filepath.Join(d.BundleDir(), "*.tar.gz.pending"))
	sort.Strings(markers)
	var sent []string
	for _, m := range markers {
		bundle := strings.TrimSuffix(m, ".pending")
		if _, err := os.Stat(bundle); err != nil {
			_ = os.Remove(m)
			continue
		}
		id, err := d.Upload(ctx, bundle)
		if err != nil {
			return sent, err
		}
		sent = append(sent, id)
	}
	return sent, nil
}

// LatestBundle returns the newest bundle path, or "".
func (d *Diag) LatestBundle() string {
	paths, _ := filepath.Glob(filepath.Join(d.BundleDir(), "SBL-*.tar.gz"))
	latest, latestTime := "", time.Time{}
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(latestTime) {
			latest, latestTime = p, info.ModTime()
		}
	}
	return latest
}
