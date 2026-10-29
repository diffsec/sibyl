// Package download fetches GGUF models from Hugging Face into the models
// directory. Jobs run one at a time. Each file is downloaded into a hidden
// partial directory, resumed with HTTP Range after a failure, and checked
// against its SHA-256 before the directory is renamed into place. The model
// scanner skips hidden directories, so no half-downloaded model is ever listed.
package download

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/diffsec/sibyl/internal/hf"
	"github.com/diffsec/sibyl/internal/models"
)

// Status is a job state.
type Status string

const (
	Queued      Status = "queued"
	Downloading Status = "downloading"
	Verifying   Status = "verifying"
	Done        Status = "done"
	Failed      Status = "failed"
	Canceled    Status = "canceled"
)

// headroom is the free space left on the disk after a download.
const headroom = 2 << 30

// Job is one model download.
type Job struct {
	ID         string    `json:"id"`
	Repo       string    `json:"repo"`
	Revision   string    `json:"revision"`
	Variant    string    `json:"variant"`
	Dir        string    `json:"dir"`            // final directory name under the models directory or pool
	Pool       string    `json:"pool,omitempty"` // empty for the models directory
	Files      []hf.File `json:"files"`
	Status     Status    `json:"status"`
	BytesDone  int64     `json:"bytes_done"`
	BytesTotal int64     `json:"bytes_total"`
	Error      string    `json:"error,omitempty"`
	Created    time.Time `json:"created"`
	Updated    time.Time `json:"updated"`
}

// Manager runs download jobs.
type Manager struct {
	ModelsDir string
	// PoolsDir holds one directory per model pool. A job can download into a pool.
	PoolsDir  string
	StateFile string
	HF        *hf.Client
	Log       *slog.Logger
	// OnDone runs after a model is moved into place, so the caller can
	// regenerate the llama-swap configuration.
	OnDone func()
	// HTTP downloads file bodies. It must not have an overall timeout.
	HTTP *http.Client
	// StallTimeout aborts and retries an attempt that receives no data.
	StallTimeout time.Duration

	mu      sync.Mutex
	jobs    map[string]*Job
	cancels map[string]context.CancelFunc
	wake    chan struct{}
}

var dirUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// DirName turns a variant name into a folder name. The folder and the file
// base name match, so the model gets a short ID (see models.ID).
func DirName(variant string) string {
	d := dirUnsafe.ReplaceAllString(filepath.Base(variant), "-")
	return strings.Trim(d, "-.")
}

// Start loads saved jobs, requeues unfinished ones, and starts the worker.
func (m *Manager) Start(ctx context.Context) error {
	m.jobs = map[string]*Job{}
	m.cancels = map[string]context.CancelFunc{}
	m.wake = make(chan struct{}, 1)
	if m.StallTimeout == 0 {
		m.StallTimeout = 2 * time.Minute
	}
	if m.HTTP == nil {
		m.HTTP = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: time.Minute,
			TLSHandshakeTimeout:   30 * time.Second,
		}}
	}
	data, err := os.ReadFile(m.StateFile)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		var saved []*Job
		if err := json.Unmarshal(data, &saved); err != nil {
			return fmt.Errorf("parse %s: %w", m.StateFile, err)
		}
		for _, j := range saved {
			if j.Status == Downloading || j.Status == Verifying {
				j.Status = Queued
			}
			m.jobs[j.ID] = j
		}
	}
	go m.worker(ctx)
	m.poke()
	return nil
}

func (m *Manager) poke() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// List returns every job, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.After(out[b].Created) })
	return out
}

// Get returns one job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Add queues a download of one variant into the models directory, or into
// a pool. An empty revision means the current commit of main, pinned now so
// every file comes from one commit.
func (m *Manager) Add(ctx context.Context, repo, revision, variant, pool string) (Job, error) {
	if !hf.ValidRepo(repo) {
		return Job{}, fmt.Errorf("invalid repository %q", repo)
	}
	if pool != "" {
		if fi, err := os.Stat(filepath.Join(m.PoolsDir, pool)); !models.PoolRE.MatchString(pool) || err != nil || !fi.IsDir() {
			return Job{}, fmt.Errorf("no model pool %q", pool)
		}
	}
	if revision == "" {
		rev, err := m.HF.Revision(ctx, repo)
		if err != nil {
			return Job{}, err
		}
		revision = rev
	}
	if !hf.ValidRevision(revision) {
		return Job{}, fmt.Errorf("revision must be a 40-character commit SHA, got %q", revision)
	}
	files, err := m.HF.Files(ctx, repo, revision)
	if err != nil {
		return Job{}, err
	}
	v, err := hf.FindVariant(hf.Variants(files), variant)
	if err != nil {
		return Job{}, err
	}
	dir := DirName(v.Name)
	if dir == "" {
		return Job{}, fmt.Errorf("variant %q has no usable name", v.Name)
	}
	if _, err := os.Stat(filepath.Join(m.root(pool), dir)); err == nil {
		return Job{}, fmt.Errorf("%s is already installed", dir)
	}

	m.mu.Lock()
	for _, j := range m.jobs {
		if j.Dir == dir && (j.Status == Queued || j.Status == Downloading || j.Status == Verifying) {
			m.mu.Unlock()
			return Job{}, fmt.Errorf("%s is already downloading (job %s)", dir, j.ID)
		}
	}
	now := time.Now().UTC()
	j := &Job{
		ID: newID(), Repo: repo, Revision: revision, Variant: v.Name, Dir: dir,
		Pool: pool, Files: v.Files, Status: Queued, BytesTotal: v.Size, Created: now, Updated: now,
	}
	m.jobs[j.ID] = j
	// The worker can change the job as soon as the lock is free.
	out := *j
	m.mu.Unlock()

	if err := m.save(); err != nil {
		return Job{}, err
	}
	m.poke()
	return out, nil
}

// Cancel stops a job and deletes its partial files.
func (m *Manager) Cancel(id string) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no job %q", id)
	}
	if j.Status == Done || j.Status == Failed || j.Status == Canceled {
		m.mu.Unlock()
		return fmt.Errorf("job %s has already finished (%s)", id, j.Status)
	}
	j.Status = Canceled
	j.Updated = time.Now().UTC()
	if cancel := m.cancels[id]; cancel != nil {
		cancel()
	}
	partial := m.partialDir(j)
	m.mu.Unlock()
	_ = os.RemoveAll(partial)
	return m.save()
}

// Retry requeues a failed job. Its partial files are kept, so it resumes.
func (m *Manager) Retry(id string) error {
	m.mu.Lock()
	j, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("no job %q", id)
	}
	if j.Status != Failed {
		m.mu.Unlock()
		return fmt.Errorf("job %s is %s, not failed", id, j.Status)
	}
	j.Status = Queued
	j.Error = ""
	j.Updated = time.Now().UTC()
	m.mu.Unlock()
	if err := m.save(); err != nil {
		return err
	}
	m.poke()
	return nil
}

// root is the directory that a job downloads into.
func (m *Manager) root(pool string) string {
	if pool == "" {
		return m.ModelsDir
	}
	return filepath.Join(m.PoolsDir, pool)
}

// partialDir is on the same file system as the final directory, so the
// last step is a rename.
func (m *Manager) partialDir(j *Job) string {
	return filepath.Join(m.root(j.Pool), ".partial-"+j.ID)
}

func (m *Manager) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		}
		for {
			j := m.next()
			if j == nil {
				break
			}
			m.run(ctx, j)
		}
	}
}

func (m *Manager) next() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	var next *Job
	for _, j := range m.jobs {
		if j.Status == Queued && (next == nil || j.Created.Before(next.Created)) {
			next = j
		}
	}
	return next
}

func (m *Manager) setStatus(j *Job, s Status, errMsg string) {
	m.mu.Lock()
	if j.Status != Canceled {
		j.Status = s
		j.Error = errMsg
	}
	j.Updated = time.Now().UTC()
	m.mu.Unlock()
	if err := m.save(); err != nil {
		m.Log.Error("save download state", "err", err)
	}
}

func (m *Manager) run(parent context.Context, j *Job) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	m.mu.Lock()
	m.cancels[j.ID] = cancel
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.cancels, j.ID)
		m.mu.Unlock()
	}()

	m.setStatus(j, Downloading, "")
	m.Log.Info("download started", "job", j.ID, "repo", j.Repo, "variant", j.Variant)
	if err := m.fetchAll(ctx, j); err != nil {
		if ctx.Err() != nil && parent.Err() == nil {
			return // canceled by the user; Cancel already cleaned up
		}
		m.Log.Error("download failed", "job", j.ID, "err", err)
		m.setStatus(j, Failed, err.Error())
		return
	}
	m.setStatus(j, Done, "")
	m.Log.Info("download finished", "job", j.ID, "dir", j.Dir)
	if m.OnDone != nil {
		m.OnDone()
	}
}

func (m *Manager) fetchAll(ctx context.Context, j *Job) error {
	partial := m.partialDir(j)
	if err := os.MkdirAll(partial, 0o750); err != nil {
		return err
	}
	if err := m.checkSpace(j, partial); err != nil {
		return err
	}
	for _, f := range j.Files {
		if err := m.fetchFile(ctx, j, partial, f); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	m.setStatus(j, Verifying, "")
	for _, f := range j.Files {
		if err := verify(filepath.Join(partial, filepath.Base(f.Path)), f.SHA256); err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	source, err := json.MarshalIndent(map[string]any{
		"repo": j.Repo, "revision": j.Revision, "variant": j.Variant, "files": j.Files,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(partial, ".sibyl-source.json"), source, 0o640); err != nil {
		return err
	}
	final := filepath.Join(m.root(j.Pool), j.Dir)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("%s appeared during the download; keeping the partial files in %s", final, partial)
	}
	return os.Rename(partial, final)
}

// checkSpace fails early when the disk cannot hold the rest of the download.
func (m *Manager) checkSpace(j *Job, partial string) error {
	var have int64
	for _, f := range j.Files {
		if st, err := os.Stat(filepath.Join(partial, filepath.Base(f.Path))); err == nil {
			have += st.Size()
		}
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.root(j.Pool), &st); err != nil {
		return err
	}
	free := int64(st.Bavail) * int64(st.Bsize) // Bsize is int64 on Linux and uint32 on macOS
	need := j.BytesTotal - have + headroom
	if free < need {
		return fmt.Errorf("not enough disk space: need %d GiB, %d GiB free", need>>30, free>>30)
	}
	return nil
}

// fetchFile downloads one file, resuming from a partial file and retrying
// with backoff. Each attempt fails if no data arrives for StallTimeout.
func (m *Manager) fetchFile(ctx context.Context, j *Job, dir string, f hf.File) error {
	dest := filepath.Join(dir, filepath.Base(f.Path))
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt*attempt) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		done, err := m.attempt(ctx, j, dest, f)
		if done {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A refusal does not change on retry: the user must set a token
		// or accept the repository's terms first.
		if errors.Is(err, hf.ErrAccess) {
			return err
		}
		lastErr = err
		m.Log.Warn("download attempt failed", "job", j.ID, "file", f.Path, "attempt", attempt+1, "err", err)
	}
	return lastErr
}

func (m *Manager) attempt(parent context.Context, j *Job, dest string, f hf.File) (bool, error) {
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return false, err
	}
	defer func() { _ = out.Close() }()
	have, err := out.Seek(0, io.SeekEnd)
	if err != nil {
		return false, err
	}
	if have == f.Size {
		return true, nil
	}
	if have > f.Size {
		if err := out.Truncate(0); err != nil {
			return false, err
		}
		have = 0
	}

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.HF.FileURL(j.Repo, j.Revision, f.Path), nil)
	if err != nil {
		return false, err
	}
	m.HF.Authorize(req)
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusPartialContent && have > 0:
	case resp.StatusCode == http.StatusOK:
		if have > 0 { // the server ignored the Range header; start over
			if err := out.Truncate(0); err != nil {
				return false, err
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return false, err
			}
			have = 0
		}
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, hf.ErrAccess
	default:
		return false, fmt.Errorf("unexpected HTTP status %s", resp.Status)
	}

	stall := time.AfterFunc(m.StallTimeout, cancel)
	defer stall.Stop()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(m.StallTimeout)
			if _, err := out.Write(buf[:n]); err != nil {
				return false, err
			}
			have += int64(n)
			m.progress(j)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return false, rerr
		}
	}
	if have != f.Size {
		return false, fmt.Errorf("got %d of %d bytes", have, f.Size)
	}
	return true, nil
}

// progress recomputes BytesDone from the partial files on disk.
func (m *Manager) progress(j *Job) {
	var done int64
	for _, f := range j.Files {
		if st, err := os.Stat(filepath.Join(m.partialDir(j), filepath.Base(f.Path))); err == nil {
			done += st.Size()
		}
	}
	m.mu.Lock()
	j.BytesDone = done
	m.mu.Unlock()
}

func verify(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		_ = os.Remove(path) // a corrupt file must be downloaded again
		return fmt.Errorf("SHA-256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

func (m *Manager) save() error {
	m.mu.Lock()
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		c := *j
		jobs = append(jobs, &c)
	}
	m.mu.Unlock()
	sort.Slice(jobs, func(a, b int) bool { return jobs[a].Created.Before(jobs[b].Created) })
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.StateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, m.StateFile)
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
