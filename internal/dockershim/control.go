package dockershim

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	apitypes "github.com/moby/buildkit/api/types"
	"github.com/moby/buildkit/solver/pb"
	digest "github.com/opencontainers/go-digest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// control is the part of the BuildKit control API that a client needs for
// a Dockerfile build: Info, ListWorkers, Solve and Status.
type control struct {
	controlapi.UnimplementedControlServer
	s    *Shim
	mu   sync.Mutex
	jobs map[string]*job
	// keep is how long the status of a finished build stays readable.
	keep time.Duration
}

func newControl(s *Shim) *control {
	return &control{s: s, jobs: map[string]*job{}, keep: time.Minute}
}

// job holds the status events of one build. Status can start before Solve.
type job struct {
	mu     sync.Mutex
	events []*controlapi.StatusResponse
	done   bool
	wake   chan struct{}
}

func (j *job) publish(ev *controlapi.StatusResponse, done bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if ev != nil {
		j.events = append(j.events, ev)
	}
	j.done = j.done || done
	close(j.wake)
	j.wake = make(chan struct{})
}

func (c *control) job(ref string) *job {
	c.mu.Lock()
	defer c.mu.Unlock()
	j, ok := c.jobs[ref]
	if !ok {
		j = &job{wake: make(chan struct{})}
		c.jobs[ref] = j
	}
	return j
}

func (c *control) Info(context.Context, *controlapi.InfoRequest) (*controlapi.InfoResponse, error) {
	return &controlapi.InfoResponse{BuildkitVersion: &apitypes.BuildkitVersion{
		Package: "github.com/diffsec/sibyl/internal/dockershim",
		Version: "podman",
	}}, nil
}

func (c *control) ListWorkers(context.Context, *controlapi.ListWorkersRequest) (*controlapi.ListWorkersResponse, error) {
	return &controlapi.ListWorkersResponse{Record: []*apitypes.WorkerRecord{{
		ID:        "podman",
		Platforms: []*pb.Platform{{OS: "linux", Architecture: runtime.GOARCH}},
	}}}, nil
}

func (c *control) Status(req *controlapi.StatusRequest, stream controlapi.Control_StatusServer) error {
	j := c.job(req.Ref)
	for sent := 0; ; {
		j.mu.Lock()
		evs, done, wake := j.events[sent:], j.done, j.wake
		j.mu.Unlock()
		for _, ev := range evs {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		sent += len(evs)
		if done {
			return nil
		}
		select {
		case <-wake:
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func (c *control) Solve(ctx context.Context, req *controlapi.SolveRequest) (*controlapi.SolveResponse, error) {
	if req.Frontend != "dockerfile.v0" || req.Definition != nil {
		return nil, status.Errorf(codes.Unimplemented, "only Dockerfile builds work on Podman, not frontend %q", req.Frontend)
	}
	tags, err := exportTags(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	j := c.job(req.Ref)
	defer func() {
		j.publish(nil, true)
		time.AfterFunc(c.keep, func() {
			c.mu.Lock()
			delete(c.jobs, req.Ref)
			c.mu.Unlock()
		})
	}()
	v := &controlapi.Vertex{
		Digest:  digest.FromString("sibyl-docker-shim/" + req.Ref).String(),
		Name:    "podman build",
		Started: timestamppb.Now(),
	}
	j.publish(&controlapi.StatusResponse{Vertexes: []*controlapi.Vertex{v}}, false)
	logf := func(b []byte) {
		j.publish(&controlapi.StatusResponse{Logs: []*controlapi.VertexLog{{
			Vertex: v.Digest, Timestamp: timestamppb.Now(), Stream: 1, Msg: b,
		}}}, false)
	}
	id, err := c.s.build(ctx, req, tags, logf)
	done := &controlapi.Vertex{Digest: v.Digest, Name: v.Name, Started: v.Started, Completed: timestamppb.Now()}
	if err != nil {
		done.Error = err.Error()
	}
	j.publish(&controlapi.StatusResponse{Vertexes: []*controlapi.Vertex{done}}, false)
	if err != nil {
		return nil, err
	}
	resp := map[string]string{"containerimage.digest": id}
	if len(tags) > 0 {
		resp["image.name"] = strings.Join(tags, ",")
	}
	return &controlapi.SolveResponse{ExporterResponse: resp}, nil
}

// exportTags reads the image names from the exporters. Only the moby
// exporter, which stores the image in the engine, works on Podman.
func exportTags(req *controlapi.SolveRequest) ([]string, error) {
	exps := req.Exporters
	if len(exps) == 0 && req.ExporterDeprecated != "" {
		exps = []*controlapi.Exporter{{Type: req.ExporterDeprecated, Attrs: req.ExporterAttrsDeprecated}}
	}
	var tags []string
	for _, e := range exps {
		if e.Type != "moby" {
			return nil, &unsupportedError{"the " + e.Type + " exporter"}
		}
		for t := range strings.SplitSeq(e.Attrs["name"], ",") {
			if t = strings.TrimSpace(t); t != "" {
				tags = append(tags, t)
			}
		}
	}
	return tags, nil
}

type unsupportedError struct{ what string }

func (e *unsupportedError) Error() string { return e.what + " does not work on Podman" }
