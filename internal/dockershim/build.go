package dockershim

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/auth"
	"github.com/moby/buildkit/session/filesync"
	fstypes "github.com/tonistiigi/fsutil/types"
)

// shimDockerfile is the name of the Dockerfile in the context that the shim
// sends to Podman.
const shimDockerfile = ".sibyl-shim.Dockerfile"

// build pulls the build context through the session, builds it with Podman,
// and returns the image ID.
func (s *Shim) build(ctx context.Context, req *controlapi.SolveRequest, tags []string, logf func([]byte)) (string, error) {
	caller, err := s.sm.Get(ctx, req.Session, false)
	if err != nil {
		return "", fmt.Errorf("build session %s: %w", req.Session, err)
	}
	attrs := req.FrontendAttrs
	name, err := dockerfileName(attrs["filename"])
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "sibyl-build-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctxDir, dfDir := filepath.Join(dir, "context"), filepath.Join(dir, "dockerfile")
	if err := fetchDir(ctx, caller, or(attrs["contextkey"], "context"), ctxDir, nil); err != nil {
		return "", fmt.Errorf("build context: %w", err)
	}
	if err := fetchDir(ctx, caller, or(attrs["dockerfilekey"], "dockerfile"), dfDir, []string{name, name + ".dockerignore"}); err != nil {
		return "", fmt.Errorf("dockerfile: %w", err)
	}
	df, err := os.ReadFile(filepath.Join(dfDir, filepath.FromSlash(name)))
	if err != nil {
		return "", fmt.Errorf("dockerfile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(ctxDir, shimDockerfile), df, 0o644); err != nil {
		return "", err
	}
	if err := writeIgnore(ctxDir, filepath.Join(dfDir, filepath.FromSlash(name)+".dockerignore")); err != nil {
		return "", err
	}
	q, err := buildQuery(attrs, tags)
	if err != nil {
		return "", err
	}
	creds := s.registryConfig(ctx, req.Session, fromHosts(string(df)))
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(writeTar(pw, ctxDir)) }()
	defer func() { _ = pr.Close() }()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://podman/v1.41/build?"+q.Encode(), pr)
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", "application/x-tar")
	if creds != "" {
		hreq.Header.Set("X-Registry-Config", creds)
	}
	resp, err := s.podman.Do(hreq)
	if err != nil {
		return "", fmt.Errorf("podman build: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("podman build: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	id, err := readBuildStream(resp.Body, logf)
	if err != nil {
		return "", err
	}
	if id == "" && len(tags) > 0 {
		id = s.imageID(ctx, tags[0])
	}
	return id, nil
}

// writeIgnore writes the .dockerignore file that Podman applies. A
// Dockerfile-specific ignore file replaces the context's one, as in
// BuildKit. The last line keeps the shim's Dockerfile out of COPY and ADD.
func writeIgnore(ctxDir, dfIgnore string) error {
	p := filepath.Join(ctxDir, ".dockerignore")
	ign, err := os.ReadFile(dfIgnore) // #nosec G304 -- a file in the shim's own build folder
	if errors.Is(err, os.ErrNotExist) {
		ign, err = os.ReadFile(p) // #nosec G304 -- a file in the shim's own build folder
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(ign) > 0 && ign[len(ign)-1] != '\n' {
		ign = append(ign, '\n')
	}
	return os.WriteFile(p, append(ign, shimDockerfile+"\n"...), 0o644) // #nosec G306 -- Podman reads it
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// dockerfileName checks the Dockerfile name that the client asks for.
func dockerfileName(name string) (string, error) {
	name = path.Clean("/" + filepath.ToSlash(or(name, "Dockerfile")))[1:]
	if name == "" || name == "." {
		return "", errors.New("the Dockerfile name is empty")
	}
	return name, nil
}

// fetchDir copies a folder of the client into dir. The client sends every file
// as owned by root. The shim runs without root, so it keeps the files as
// its own, and Podman sets the owner in the image.
func fetchDir(ctx context.Context, caller session.Caller, name, dir string, include []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid()) // #nosec G115 -- ids fit in uint32
	return filesync.FSSync(ctx, caller, filesync.FSSendRequestOpt{
		Name:            name,
		DestDir:         dir,
		IncludePatterns: include,
		Filter: func(_ string, st *fstypes.Stat) bool {
			st.Uid, st.Gid, st.Xattrs = uid, gid, nil
			return true
		},
	})
}

// buildQuery turns the Dockerfile frontend options into Podman /build
// parameters.
func buildQuery(attrs map[string]string, tags []string) (url.Values, error) {
	q := url.Values{}
	q.Set("dockerfile", shimDockerfile)
	q.Set("rm", "1")
	for _, t := range tags {
		q.Add("t", t)
	}
	args, labels := map[string]string{}, map[string]string{}
	for k, v := range attrs {
		if a, ok := strings.CutPrefix(k, "build-arg:"); ok {
			args[a] = v
		} else if l, ok := strings.CutPrefix(k, "label:"); ok {
			labels[l] = v
		}
	}
	for k, m := range map[string]map[string]string{"buildargs": args, "labels": labels} {
		if len(m) == 0 {
			continue
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		q.Set(k, string(b))
	}
	if t := attrs["target"]; t != "" {
		q.Set("target", t)
	}
	if _, ok := attrs["no-cache"]; ok {
		q.Set("nocache", "1")
	}
	if attrs["image-resolve-mode"] == "pull" {
		q.Set("pull", "1")
	}
	if p := attrs["platform"]; p != "" {
		q.Set("platform", p)
	}
	if n := attrs["force-network-mode"]; n == "host" || n == "none" {
		q.Set("networkmode", n)
	}
	return q, nil
}

// writeTar writes dir as a tar stream.
func writeTar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := fi.Mode()
		if !mode.IsRegular() && !mode.IsDir() && mode&fs.ModeSymlink == 0 {
			return nil // sockets, pipes and devices do not go in a build context
		}
		link := ""
		if mode&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		h.Name = filepath.ToSlash(rel)
		if mode.IsDir() {
			h.Name += "/"
		}
		h.Uid, h.Gid, h.Uname, h.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if !mode.IsRegular() {
			return nil
		}
		f, err := os.Open(p) // #nosec G304 -- a file of the build context that the shim wrote
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return errors.Join(err, f.Close())
	})
	return errors.Join(err, tw.Close())
}

// buildMessage is one line of the Docker build output stream.
type buildMessage struct {
	Stream      string                    `json:"stream"`
	Status      string                    `json:"status"`
	Progress    string                    `json:"progress"`
	ID          string                    `json:"id"`
	Error       string                    `json:"error"`
	ErrorDetail *struct{ Message string } `json:"errorDetail"`
	Aux         json.RawMessage           `json:"aux"`
}

// readBuildStream sends the build output to logf, and returns the image ID
// or the build error.
func readBuildStream(r io.Reader, logf func([]byte)) (string, error) {
	dec := json.NewDecoder(bufio.NewReader(r))
	var id string
	for {
		var m buildMessage
		if err := dec.Decode(&m); err == io.EOF {
			return id, nil
		} else if err != nil {
			return "", fmt.Errorf("podman build output: %w", err)
		}
		switch {
		case m.Error != "" || m.ErrorDetail != nil:
			msg := m.Error
			if msg == "" {
				msg = m.ErrorDetail.Message
			}
			return "", errors.New(strings.TrimSpace(msg))
		case m.Stream != "":
			logf([]byte(m.Stream))
		case m.Status != "":
			line := strings.TrimSpace(strings.Join([]string{m.ID, m.Status, m.Progress}, " "))
			logf([]byte(line + "\n"))
		case len(m.Aux) > 0:
			var aux struct{ ID string }
			if json.Unmarshal(m.Aux, &aux) == nil && aux.ID != "" {
				id = aux.ID
			}
		}
	}
}

// imageID asks Podman for the ID of an image.
func (s *Shim) imageID(ctx context.Context, ref string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://podman/v1.41/images/"+url.PathEscape(ref)+"/json", nil)
	if err != nil {
		return ""
	}
	resp, err := s.podman.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var img struct {
		ID string `json:"Id"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&img) != nil {
		return ""
	}
	return img.ID
}

// fromHosts returns the registry of each base image in a Dockerfile, so the
// shim can ask the client for its logins.
func fromHosts(dockerfile string) []string {
	stages := map[string]bool{}
	seen := map[string]bool{}
	var hosts []string
	for _, line := range strings.Split(strings.ReplaceAll(dockerfile, "\\\n", " "), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.EqualFold(f[0], "FROM") {
			continue
		}
		args := f[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		img := args[0]
		earlier := stages[strings.ToLower(img)]
		if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
			stages[strings.ToLower(args[2])] = true
		}
		// A stage name, scratch, or a name from an ARG has no registry to log in to.
		if earlier || img == "scratch" || strings.Contains(img, "$") {
			continue
		}
		if h := registryHost(img); !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// registryHost returns the registry of an image reference.
func registryHost(ref string) string {
	host, _, ok := strings.Cut(ref, "/")
	if !ok || (!strings.ContainsAny(host, ".:") && host != "localhost") {
		return "docker.io"
	}
	return host
}

// registryConfig asks the client session for the login of each registry,
// and returns them as an X-Registry-Config header. Podman pulls the base
// images with them.
func (s *Shim) registryConfig(ctx context.Context, sessionID string, hosts []string) string {
	type authConfig struct {
		Username      string `json:"username,omitempty"`
		Password      string `json:"password,omitempty"`
		IdentityToken string `json:"identitytoken,omitempty"`
	}
	creds := auth.CredentialsFunc(ctx, s.sm, session.NewGroup(sessionID))
	cfg := map[string]authConfig{}
	for _, h := range hosts {
		lookup, key := h, h
		if h == "docker.io" {
			lookup, key = "registry-1.docker.io", "https://index.docker.io/v1/"
		}
		_, user, secret, err := creds(lookup)
		if err != nil || secret == "" {
			continue
		}
		if user == "" {
			cfg[key] = authConfig{IdentityToken: secret}
		} else {
			cfg[key] = authConfig{Username: user, Password: secret}
		}
	}
	if len(cfg) == 0 {
		return ""
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(b)
}
