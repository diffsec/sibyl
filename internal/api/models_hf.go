package api

import (
	"bufio"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/ihavespoons/sibyl/internal/gpu"
	"github.com/ihavespoons/sibyl/internal/hardware"
	"github.com/ihavespoons/sibyl/internal/hf"
)

// Fit says how a model of a given size can run on this host.
type Fit string

const (
	// FitGPU means the whole file fits in the selected GPUs' VRAM, with room for the KV cache.
	FitGPU Fit = "gpu"
	// FitSplit means the file needs system RAM as well. Models whose token
	// embedding table is large (llama.cpp keeps it in RAM) can still run
	// fully on GPU; check the model card.
	FitSplit Fit = "split"
	// FitNo means the file is larger than VRAM plus half of system RAM.
	FitNo Fit = "no"
	// FitUnknown means a selected GPU does not report its VRAM. amdgpu does
	// in sysfs; the NVIDIA and Intel drivers do not.
	FitUnknown Fit = "unknown"
)

// kvReserve is the VRAM per GPU left for the KV cache and compute buffers.
const kvReserve = 2 << 30

type variantView struct {
	hf.Variant
	Fit Fit `json:"fit"`
}

type repoView struct {
	Repo     string        `json:"repo"`
	Revision string        `json:"revision"`
	VRAM     int64         `json:"vram_bytes"`
	RAM      int64         `json:"ram_bytes"`
	Variants []variantView `json:"variants"`
}

func (s *Server) hfSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		writeError(w, http.StatusBadRequest, errString("q: give a search term of 1 to 200 characters"))
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	res, err := s.HF.Search(r.Context(), q, limit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) hfRepo(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	if !hf.ValidRepo(repo) {
		writeError(w, http.StatusBadRequest, errString("invalid repository name"))
		return
	}
	rev := r.URL.Query().Get("revision")
	if rev == "" {
		var err error
		if rev, err = s.HF.Revision(r.Context(), repo); err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
	} else if !hf.ValidRevision(rev) {
		writeError(w, http.StatusBadRequest, errString("revision must be a 40-character commit SHA"))
		return
	}
	files, err := s.HF.Files(r.Context(), repo, rev)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	vram, ram, known := s.memory()
	out := repoView{Repo: repo, Revision: rev, VRAM: vram, RAM: ram}
	for _, v := range hf.Variants(files) {
		f := FitUnknown
		if known {
			f = fit(v.Size, vram, ram)
		}
		out.Variants = append(out.Variants, variantView{Variant: v, Fit: f})
	}
	writeJSON(w, http.StatusOK, out)
}

func fit(size, vram, ram int64) Fit {
	switch {
	case vram > 0 && size <= vram:
		return FitGPU
	case size <= vram+ram/2:
		return FitSplit
	default:
		return FitNo
	}
}

// memory returns usable VRAM (selected GPUs, less the KV reserve) and total
// RAM. known is false when a selected GPU does not report its VRAM.
func (s *Server) memory() (vram, ram int64, known bool) {
	gpus, _ := gpu.Detect(s.Paths.SysfsRoot)
	hw, err := hardware.Load(s.Paths.hardwareFile(), s.Variant, gpus)
	known = err == nil
	if err == nil {
		for _, i := range hw.GPUs {
			if i < 0 || i >= len(gpus) || gpus[i].VRAMTotal == 0 {
				known = false
				continue
			}
			if gpus[i].VRAMTotal > kvReserve {
				vram += int64(gpus[i].VRAMTotal) - kvReserve
			}
		}
	}
	if f, err := os.Open(s.Paths.MemInfo); err == nil {
		defer func() { _ = f.Close() }()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if fields := strings.Fields(sc.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
				kb, _ := strconv.ParseInt(fields[1], 10, 64)
				ram = kb << 10
			}
		}
	}
	return vram, ram, known
}

type downloadRequest struct {
	Repo     string `json:"repo"`
	Revision string `json:"revision,omitempty"`
	Variant  string `json:"variant"`
	Pool     string `json:"pool,omitempty"`
}

func (s *Server) addDownload(w http.ResponseWriter, r *http.Request) {
	var req downloadRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	j, err := s.Downloads.Add(r.Context(), req.Repo, req.Revision, req.Variant, req.Pool)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusAccepted, j)
}

func (s *Server) listDownloads(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Downloads.List())
}

func (s *Server) getDownload(w http.ResponseWriter, r *http.Request) {
	j, ok := s.Downloads.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errString("no such download"))
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) cancelDownload(w http.ResponseWriter, r *http.Request) {
	if err := s.Downloads.Cancel(r.PathValue("id")); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceled"})
}

func (s *Server) retryDownload(w http.ResponseWriter, r *http.Request) {
	if err := s.Downloads.Retry(r.PathValue("id")); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// OnDownloadDone regenerates the configuration after a model lands on disk.
func (s *Server) OnDownloadDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.Apply(); err != nil {
		s.Log.Error("apply after download", "err", err)
	}
	go s.SyncOpenWebUI()
}

type errString string

func (e errString) Error() string { return string(e) }
