package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ihavespoons/sibyl/internal/hostctl"
	"github.com/ihavespoons/sibyl/internal/hostd"
)

// defaultHostWait is how long a host route waits for sibyl-hostd before it
// returns 202 with the request ID. It is below the server's write timeout.
const defaultHostWait = 45 * time.Second

func (s *Server) getHost(w http.ResponseWriter, _ *http.Request) {
	if s.HostAgent == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("host actions are not configured"))
		return
	}
	st, err := s.HostAgent.State()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) getHostRequest(w http.ResponseWriter, r *http.Request) {
	if s.HostAgent == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("host actions are not configured"))
		return
	}
	res, err := s.HostAgent.Result(r.PathValue("id"))
	switch {
	case errors.Is(err, hostctl.ErrPending):
		writeJSON(w, http.StatusAccepted, map[string]string{"id": r.PathValue("id"), "status": "queued"})
	case err != nil:
		writeError(w, http.StatusBadRequest, err)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) putHostname(w http.ResponseWriter, r *http.Request) {
	var h hostd.Hostname
	if err := readJSON(r, &h); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.hostAction(w, r, hostd.SetHostname, h)
}

func (s *Server) putNetwork(w http.ResponseWriter, r *http.Request) {
	var n hostd.Network
	if err := readJSON(r, &n); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	n.Interface = r.PathValue("iface")
	s.hostAction(w, r, hostd.SetNetwork, n)
}

func (s *Server) confirmNetwork(w http.ResponseWriter, r *http.Request) {
	s.hostAction(w, r, hostd.ConfirmNetwork, struct{}{})
}

// userBody is the body of PUT /host/users/{name}. GitHub, if set, adds the
// public keys that github.com publishes for that account.
type userBody struct {
	Keys   []string `json:"keys"`
	GitHub string   `json:"github,omitempty"`
}

func (s *Server) putUser(w http.ResponseWriter, r *http.Request) {
	var b userBody
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	u := hostd.User{Name: r.PathValue("name"), Keys: b.Keys}
	if b.GitHub != "" {
		keys, err := s.githubKeys(r.Context(), b.GitHub)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		u.Keys = append(u.Keys, keys...)
	}
	s.hostAction(w, r, hostd.SetUser, u)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	s.hostAction(w, r, hostd.DeleteUser, hostd.UserName{Name: r.PathValue("name")})
}

// hostAction queues one request and waits for its result. A request that
// fails validation returns 422 and is never queued.
func (s *Server) hostAction(w http.ResponseWriter, r *http.Request, a hostd.Action, body any) {
	if s.HostAgent == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("host actions are not configured"))
		return
	}
	id, err := s.HostAgent.Submit(a, body)
	var inv *hostctl.InvalidError
	if errors.As(err, &inv) {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	wait := s.HostWait
	if wait == 0 {
		wait = defaultHostWait
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	res, err := s.HostAgent.Wait(ctx, id)
	switch {
	case errors.Is(err, hostctl.ErrPending):
		writeJSON(w, http.StatusAccepted, map[string]string{"id": id, "status": "queued"})
	case err != nil:
		writeError(w, http.StatusInternalServerError, err)
	case !res.OK:
		writeJSON(w, http.StatusUnprocessableEntity, res)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) githubKeys(ctx context.Context, user string) ([]string, error) {
	base := s.GitHubBase
	if base == "" {
		base = "https://github.com"
	}
	return hostd.GitHubKeys(ctx, base, user)
}
