// Package hf is a small client for the Hugging Face Hub API: search GGUF
// repositories, list a repository's GGUF files, and build download URLs.
package hf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/models"
)

// DefaultBase is the Hub address.
const DefaultBase = "https://huggingface.co"

var (
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}/[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
	revisionRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
	sha256Re   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	gitOIDRe   = regexp.MustCompile(`^[0-9a-f]{40}$`) // a Git blob ID: SHA-1
	splitRe    = regexp.MustCompile(`^(.*)-([0-9]{5})-of-([0-9]{5})\.gguf$`)
)

// ValidRepo reports whether id looks like "owner/name".
func ValidRepo(id string) bool { return repoRe.MatchString(id) && !strings.Contains(id, "..") }

// ValidRevision reports whether rev is a full commit SHA.
func ValidRevision(rev string) bool { return revisionRe.MatchString(rev) }

// Client talks to the Hub.
type Client struct {
	Base string
	HTTP *http.Client

	mu    sync.RWMutex
	token string // optional, for gated and private repositories
}

// New returns a Client for base with request timeouts suited to API calls.
func New(base, token string) *Client {
	return &Client{Base: strings.TrimSuffix(base, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}, token: token}
}

// SetToken changes the access token for later requests. An empty token
// signs out.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

// HasToken reports whether requests carry a token.
func (c *Client) HasToken() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.token != ""
}

// Authorize adds the token, if any, to a request to the Hub.
func (c *Client) Authorize(req *http.Request) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// ErrAccess means the Hub refused a request: the repository is gated or
// private, and the token is missing or has no access.
var ErrAccess = errors.New("hugging face refused access: the repository is gated or private. " +
	"Accept its terms on huggingface.co with your account, and set an access token in Sibyl")

// WhoAmI returns the account name for a token, which it checks with the Hub.
func (c *Client) WhoAmI(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/api/whoami-v2", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return "", errors.New("hugging face rejected the token: check that it is a valid access token")
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("hugging face: %s", resp.Status)
	}
	var who struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&who); err != nil {
		return "", err
	}
	if who.Name == "" {
		return "", errors.New("hugging face returned no account name for the token")
	}
	return who.Name, nil
}

// Summary is one search result.
type Summary struct {
	ID        string `json:"id"`
	Downloads int    `json:"downloads"`
	Likes     int    `json:"likes"`
	Gated     any    `json:"gated,omitempty"`
	Modified  string `json:"lastModified,omitempty"`
}

// File is one file in a repository at one revision.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	// GitOID is the Git blob ID of a file that is not in LFS. Hugging Face
	// gives a SHA-256 for LFS files only, so a small file is checked
	// against this SHA-1 of its Git blob.
	GitOID string `json:"git_oid,omitempty"`
	// Dest is the file's path inside the model folder. An empty Dest means
	// the base name of Path, which is where a GGUF file goes.
	Dest string `json:"dest,omitempty"`
}

// Variant is one downloadable model: a single GGUF file, a full split set,
// or a whole Edge0 model folder.
type Variant struct {
	Name   string `json:"name"`
	Size   int64  `json:"size_bytes"`
	Shards int    `json:"shards"`
	Files  []File `json:"files"`
	// Engine is "edge0" for an Edge0 model folder, and empty for GGUF.
	Engine string `json:"engine,omitempty"`
	// RAM is the system memory that an Edge0 model keeps while it runs.
	RAM int64 `json:"ram_bytes,omitempty"`
}

func (c *Client) get(ctx context.Context, rawURL string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	c.Authorize(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrAccess
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("hugging face: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(v)
}

// Search finds GGUF repositories, most downloaded first.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Summary, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	q := url.Values{"search": {query}, "filter": {"gguf"}, "sort": {"downloads"}, "limit": {fmt.Sprint(limit)}}
	var out []Summary
	err := c.get(ctx, c.Base+"/api/models?"+q.Encode(), &out)
	return out, err
}

// Revision returns the commit that "main" points to.
func (c *Client) Revision(ctx context.Context, repo string) (string, error) {
	if !ValidRepo(repo) {
		return "", fmt.Errorf("invalid repository %q", repo)
	}
	var info struct {
		SHA string `json:"sha"`
	}
	if err := c.get(ctx, c.Base+"/api/models/"+repo, &info); err != nil {
		return "", err
	}
	if !ValidRevision(info.SHA) {
		return "", fmt.Errorf("hugging face returned an invalid revision %q", info.SHA)
	}
	return info.SHA, nil
}

// Files lists the GGUF files of repo at rev, with their SHA-256 digests.
func (c *Client) Files(ctx context.Context, repo, rev string) ([]File, error) {
	if !ValidRepo(repo) || !ValidRevision(rev) {
		return nil, fmt.Errorf("invalid repository %q or revision %q", repo, rev)
	}
	var tree []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		LFS  *struct {
			OID string `json:"oid"`
		} `json:"lfs"`
	}
	if err := c.get(ctx, c.Base+"/api/models/"+repo+"/tree/"+rev+"?recursive=true", &tree); err != nil {
		return nil, err
	}
	var out []File
	for _, e := range tree {
		if e.Type != "file" || !strings.HasSuffix(strings.ToLower(e.Path), ".gguf") {
			continue
		}
		if !SafePath(e.Path) || e.LFS == nil || !sha256Re.MatchString(e.LFS.OID) {
			continue // unsafe name, or no digest to verify against
		}
		out = append(out, File{Path: e.Path, Size: e.Size, SHA256: e.LFS.OID})
	}
	return out, nil
}

// Edge0Variant returns the whole-folder variant of an Edge0 repository, or
// false for any other repository. An Edge0 repository holds a strata pack at
// its root: model.json and the expert file. The variant leaves out the
// repository's assets folder, which holds pictures for the model card.
func (c *Client) Edge0Variant(ctx context.Context, repo, rev string) (Variant, bool, error) {
	if !ValidRepo(repo) || !ValidRevision(rev) {
		return Variant{}, false, fmt.Errorf("invalid repository %q or revision %q", repo, rev)
	}
	var tree []struct {
		Type string `json:"type"`
		Path string `json:"path"`
		Size int64  `json:"size"`
		OID  string `json:"oid"`
		LFS  *struct {
			OID string `json:"oid"`
		} `json:"lfs"`
	}
	if err := c.get(ctx, c.Base+"/api/models/"+repo+"/tree/"+rev+"?recursive=true", &tree); err != nil {
		return Variant{}, false, err
	}
	v := Variant{Name: path.Base(repo), Engine: models.EngineEdge0}
	var manifest, experts bool
	for _, e := range tree {
		if e.Type != "file" || strings.HasPrefix(e.Path, "assets/") {
			continue
		}
		f := File{Path: e.Path, Size: e.Size, Dest: e.Path}
		switch {
		case !SafePath(e.Path):
			continue // a hidden file such as .gitattributes, or an unsafe name
		case e.LFS != nil && sha256Re.MatchString(e.LFS.OID):
			f.SHA256 = e.LFS.OID
		case e.LFS == nil && gitOIDRe.MatchString(e.OID):
			f.GitOID = e.OID
		default:
			continue // no digest to verify against
		}
		switch e.Path {
		case "model.json":
			manifest = true
		case "experts.bin":
			experts = true
			v.RAM = models.Edge0RAM(e.Size)
		}
		v.Files = append(v.Files, f)
		v.Size += e.Size
		v.Shards++
	}
	if !manifest || !experts {
		return Variant{}, false, nil
	}
	sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Path < v.Files[j].Path })
	return v, true, nil
}

// RepoVariants lists what a repository offers for download: its GGUF
// variants, and the whole folder when the repository is an Edge0 model. The
// GGUF files of an Edge0 repository are parts of the folder, so they are not
// offered alone.
func (c *Client) RepoVariants(ctx context.Context, repo, rev string) ([]Variant, error) {
	folder, ok, err := c.Edge0Variant(ctx, repo, rev)
	if err != nil {
		return nil, err
	}
	if ok {
		return []Variant{folder}, nil
	}
	files, err := c.Files(ctx, repo, rev)
	if err != nil {
		return nil, err
	}
	return Variants(files), nil
}

// SafePath reports whether a repository path is a plain relative path.
func SafePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsAny(p, "\x00\n\r") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// Variants groups GGUF files into downloadable models. A split set is one
// variant, and only when every shard is present. Multimodal projector files
// (mmproj-*) are left out.
func Variants(files []File) []Variant {
	byName := map[string]*Variant{}
	want := map[string]int{}
	for _, f := range files {
		base := path.Base(f.Path)
		if strings.HasPrefix(strings.ToLower(base), "mmproj") {
			continue
		}
		name := strings.TrimSuffix(f.Path, path.Ext(f.Path))
		if m := splitRe.FindStringSubmatch(f.Path); m != nil {
			name = m[1]
			n, _ := strconv.Atoi(m[3]) // five digits, so it always parses
			want[name] = n
		}
		v, ok := byName[name]
		if !ok {
			v = &Variant{Name: name}
			byName[name] = v
		}
		v.Files = append(v.Files, f)
		v.Size += f.Size
		v.Shards++
	}
	var out []Variant
	for name, v := range byName {
		if n, split := want[name]; split && n != v.Shards {
			continue // incomplete split set
		}
		sort.Slice(v.Files, func(i, j int) bool { return v.Files[i].Path < v.Files[j].Path })
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindVariant returns the variant called name.
func FindVariant(vs []Variant, name string) (Variant, error) {
	for _, v := range vs {
		if v.Name == name {
			return v, nil
		}
	}
	return Variant{}, errors.New("no complete GGUF variant named " + name)
}

// FileURL is the download address of one file at one revision.
func (c *Client) FileURL(repo, rev, p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return c.Base + "/" + repo + "/resolve/" + rev + "/" + strings.Join(parts, "/")
}
