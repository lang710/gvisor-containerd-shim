/*
Copyright The containerd Authors.
Copyright 2018 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package shim

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/console"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/events"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/linux/runctypes"
	rproc "github.com/containerd/containerd/runtime/proc"
	"github.com/containerd/containerd/runtime/v1/shim"
	shimapi "github.com/containerd/containerd/runtime/v1/shim/v1"
	"github.com/containerd/typeurl"
	ptypes "github.com/gogo/protobuf/types"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runsc "github.com/google/gvisor-containerd-shim/pkg/go-runsc"
	"github.com/google/gvisor-containerd-shim/pkg/v1/proc"
	"github.com/google/gvisor-containerd-shim/pkg/v1/utils"
)

var (
	empty   = &ptypes.Empty{}
	bufPool = sync.Pool{
		New: func() interface{} {
			buffer := make([]byte, 32<<10)
			return &buffer
		},
	}
)

// Config contains shim specific configuration
type Config struct {
	Path        string
	Namespace   string
	WorkDir     string
	RuntimeRoot string
	RunscConfig map[string]string
}

// NewService returns a new shim service that can be used via GRPC
func NewService(config Config, publisher events.Publisher) (*Service, error) {
	if config.Namespace == "" {
		return nil, fmt.Errorf("shim namespace cannot be empty")
	}
	ctx := namespaces.WithNamespace(context.Background(), config.Namespace)
	ctx = log.WithLogger(ctx, logrus.WithFields(logrus.Fields{
		"namespace": config.Namespace,
		"path":      config.Path,
		"pid":       os.Getpid(),
	}))
	s := &Service{
		config:    config,
		context:   ctx,
		processes: make(map[string]rproc.Process),
		events:    make(chan interface{}, 128),
		ec:        proc.ExitCh,
	}
	go s.processExits()
	if err := s.initPlatform(); err != nil {
		return nil, errors.Wrap(err, "failed to initialized platform behavior")
	}
	go s.forward(publisher)
	return s, nil
}

// Service is the shim implementation of a remote shim over GRPC
type Service struct {
	mu sync.Mutex

	config    Config
	context   context.Context
	processes map[string]rproc.Process
	events    chan interface{}
	platform  rproc.Platform
	ec        chan proc.Exit

	// Filled by Create()
	id     string
	bundle string
}

// Create a new initial process and container with the underlying OCI runtime
func (s *Service) Create(ctx context.Context, r *shimapi.CreateTaskRequest) (_ *shimapi.CreateTaskResponse, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var mounts []proc.Mount
	for _, m := range r.Rootfs {
		mounts = append(mounts, proc.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Target:  m.Target,
			Options: m.Options,
		})
	}

	config := &proc.CreateConfig{
		ID:       r.ID,
		Bundle:   r.Bundle,
		Runtime:  r.Runtime,
		Rootfs:   mounts,
		Terminal: r.Terminal,
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Options:  r.Options,
	}
	rootfs := filepath.Join(r.Bundle, "rootfs")
	defer func() {
		if err != nil {
			if err2 := mount.UnmountAll(rootfs, 0); err2 != nil {
				log.G(ctx).WithError(err2).Warn("Failed to cleanup rootfs mount")
			}
		}
	}()
	for _, rm := range mounts {
		m := &mount.Mount{
			Type:    rm.Type,
			Source:  rm.Source,
			Options: rm.Options,
		}
		if err := m.Mount(rootfs); err != nil {
			return nil, errors.Wrapf(err, "failed to mount rootfs component %v", m)
		}
	}
	process, err := newInit(
		ctx,
		s.config.Path,
		s.config.WorkDir,
		s.config.RuntimeRoot,
		s.config.Namespace,
		s.config.RunscConfig,
		s.platform,
		config,
	)
	if err := process.Create(ctx, config); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	// save the main task id and bundle to the shim for additional requests
	s.id = r.ID
	s.bundle = r.Bundle
	pid := process.Pid()
	s.processes[r.ID] = process
	return &shimapi.CreateTaskResponse{
		Pid: uint32(pid),
	}, nil
}

// Start a process
func (s *Service) Start(ctx context.Context, r *shimapi.StartRequest) (*shimapi.StartResponse, error) {
	p, err := s.getExecProcess(r.ID)
	if err != nil {
		return nil, err
	}
	if err := p.Start(ctx); err != nil {
		return nil, err
	}
	return &shimapi.StartResponse{
		ID:  p.ID(),
		Pid: uint32(p.Pid()),
	}, nil
}

// Delete the initial process and container
func (s *Service) Delete(ctx context.Context, r *ptypes.Empty) (*shimapi.DeleteResponse, error) {
	p, err := s.getInitProcess()
	if err != nil {
		return nil, err
	}
	if err := p.Delete(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.processes, s.id)
	s.mu.Unlock()
	s.platform.Close()
	return &shimapi.DeleteResponse{
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   p.ExitedAt(),
		Pid:        uint32(p.Pid()),
	}, nil
}

// DeleteProcess deletes an exec'd process
func (s *Service) DeleteProcess(ctx context.Context, r *shimapi.DeleteProcessRequest) (*shimapi.DeleteResponse, error) {
	if r.ID == s.id {
		return nil, status.Errorf(codes.InvalidArgument, "cannot delete init process with DeleteProcess")
	}
	p, err := s.getExecProcess(r.ID)
	if err != nil {
		return nil, err
	}
	if err := p.Delete(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	delete(s.processes, r.ID)
	s.mu.Unlock()
	return &shimapi.DeleteResponse{
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   p.ExitedAt(),
		Pid:        uint32(p.Pid()),
	}, nil
}

// Exec an additional process inside the container
func (s *Service) Exec(ctx context.Context, r *shimapi.ExecProcessRequest) (*ptypes.Empty, error) {
	s.mu.Lock()

	if p := s.processes[r.ID]; p != nil {
		s.mu.Unlock()
		return nil, errdefs.ToGRPCf(errdefs.ErrAlreadyExists, "id %s", r.ID)
	}

	p := s.processes[s.id]
	s.mu.Unlock()
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrFailedPrecondition, "container must be created")
	}

	process, err := p.(*proc.Init).Exec(ctx, s.config.Path, &proc.ExecConfig{
		ID:       r.ID,
		Terminal: r.Terminal,
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Spec:     r.Spec,
	})
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	s.mu.Lock()
	s.processes[r.ID] = process
	s.mu.Unlock()
	return empty, nil
}

// ResizePty of a process
func (s *Service) ResizePty(ctx context.Context, r *shimapi.ResizePtyRequest) (*ptypes.Empty, error) {
	if r.ID == "" {
		return nil, errdefs.ToGRPCf(errdefs.ErrInvalidArgument, "id not provided")
	}
	ws := console.WinSize{
		Width:  uint16(r.Width),
		Height: uint16(r.Height),
	}
	p, err := s.getExecProcess(r.ID)
	if err != nil {
		return nil, err
	}
	if err := p.Resize(ws); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return empty, nil
}

// State returns runtime state information for a process
func (s *Service) State(ctx context.Context, r *shimapi.StateRequest) (*shimapi.StateResponse, error) {
	p, err := s.getExecProcess(r.ID)
	if err != nil {
		return nil, err
	}
	st, err := p.Status(ctx)
	if err != nil {
		return nil, err
	}
	status := task.StatusUnknown
	switch st {
	case "created":
		status = task.StatusCreated
	case "running":
		status = task.StatusRunning
	case "stopped":
		status = task.StatusStopped
	}
	sio := p.Stdio()
	return &shimapi.StateResponse{
		ID:         p.ID(),
		Bundle:     s.bundle,
		Pid:        uint32(p.Pid()),
		Status:     status,
		Stdin:      sio.Stdin,
		Stdout:     sio.Stdout,
		Stderr:     sio.Stderr,
		Terminal:   sio.Terminal,
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   p.ExitedAt(),
	}, nil
}

// Pause the container
func (s *Service) Pause(ctx context.Context, r *ptypes.Empty) (*ptypes.Empty, error) {
	return empty, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

// Resume the container
func (s *Service) Resume(ctx context.Context, r *ptypes.Empty) (*ptypes.Empty, error) {
	return empty, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

// Kill a process with the provided signal
func (s *Service) Kill(ctx context.Context, r *shimapi.KillRequest) (*ptypes.Empty, error) {
	if r.ID == "" {
		p, err := s.getInitProcess()
		if err != nil {
			return nil, err
		}
		if err := p.Kill(ctx, r.Signal, r.All); err != nil {
			return nil, errdefs.ToGRPC(err)
		}
		return empty, nil
	}

	p, err := s.getExecProcess(r.ID)
	if err != nil {
		return nil, err
	}
	if err := p.Kill(ctx, r.Signal, r.All); err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	return empty, nil
}

// ListPids returns all pids inside the container
func (s *Service) ListPids(ctx context.Context, r *shimapi.ListPidsRequest) (*shimapi.ListPidsResponse, error) {
	pids, err := s.getContainerPids(ctx, r.ID)
	if err != nil {
		return nil, errdefs.ToGRPC(err)
	}
	var processes []*task.ProcessInfo
	for _, pid := range pids {
		pInfo := task.ProcessInfo{
			Pid: pid,
		}
		for _, p := range s.processes {
			if p.Pid() == int(pid) {
				d := &runctypes.ProcessDetails{
					ExecID: p.ID(),
				}
				a, err := typeurl.MarshalAny(d)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to marshal process %d info", pid)
				}
				pInfo.Info = a
				break
			}
		}
		processes = append(processes, &pInfo)
	}
	return &shimapi.ListPidsResponse{
		Processes: processes,
	}, nil
}

// CloseIO of a process
func (s *Service) CloseIO(ctx context.Context, r *shimapi.CloseIORequest) (*ptypes.Empty, error) {
	p, err := s.getExecProcess(r.ID)
	if err != nil {
		return nil, err
	}
	if stdin := p.Stdin(); stdin != nil {
		if err := stdin.Close(); err != nil {
			return nil, errors.Wrap(err, "close stdin")
		}
	}
	return empty, nil
}

// Checkpoint the container
func (s *Service) Checkpoint(ctx context.Context, r *shimapi.CheckpointTaskRequest) (*ptypes.Empty, error) {
	return empty, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

// ShimInfo returns shim information such as the shim's pid
func (s *Service) ShimInfo(ctx context.Context, r *ptypes.Empty) (*shimapi.ShimInfoResponse, error) {
	return &shimapi.ShimInfoResponse{
		ShimPid: uint32(os.Getpid()),
	}, nil
}

// Update a running container
func (s *Service) Update(ctx context.Context, r *shimapi.UpdateTaskRequest) (*ptypes.Empty, error) {
	return empty, errdefs.ToGRPC(errdefs.ErrNotImplemented)
}

// Wait for a process to exit
func (s *Service) Wait(ctx context.Context, r *shimapi.WaitRequest) (*shimapi.WaitResponse, error) {
	p, err := s.getExecProcess(r.ID)
	if err != nil {
		return nil, err
	}
	p.Wait()

	return &shimapi.WaitResponse{
		ExitStatus: uint32(p.ExitStatus()),
		ExitedAt:   p.ExitedAt(),
	}, nil
}

func (s *Service) processExits() {
	for e := range s.ec {
		s.checkProcesses(e)
	}
}

func (s *Service) allProcesses() []rproc.Process {
	s.mu.Lock()
	defer s.mu.Unlock()

	res := make([]rproc.Process, 0, len(s.processes))
	for _, p := range s.processes {
		res = append(res, p)
	}
	return res
}

func (s *Service) checkProcesses(e proc.Exit) {
	for _, p := range s.allProcesses() {
		if p.ID() == e.ID {
			if ip, ok := p.(*proc.Init); ok {
				// Ensure all children are killed
				if err := ip.KillAll(s.context); err != nil {
					log.G(s.context).WithError(err).WithField("id", ip.ID()).
						Error("failed to kill init's children")
				}
			}
			p.SetExited(e.Status)
			s.events <- &eventstypes.TaskExit{
				ContainerID: s.id,
				ID:          p.ID(),
				Pid:         uint32(p.Pid()),
				ExitStatus:  uint32(e.Status),
				ExitedAt:    p.ExitedAt(),
			}
			return
		}
	}
}

func (s *Service) getContainerPids(ctx context.Context, id string) ([]uint32, error) {
	p, err := s.getInitProcess()
	if err != nil {
		return nil, err
	}

	ps, err := p.(*proc.Init).Runtime().Ps(ctx, id)
	if err != nil {
		return nil, err
	}
	pids := make([]uint32, 0, len(ps))
	for _, pid := range ps {
		pids = append(pids, uint32(pid))
	}
	return pids, nil
}

func (s *Service) forward(publisher events.Publisher) {
	for e := range s.events {
		if err := publisher.Publish(s.context, getTopic(s.context, e), e); err != nil {
			log.G(s.context).WithError(err).Error("post event")
		}
	}
}

// getInitProcess returns initial process
func (s *Service) getInitProcess() (rproc.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.processes[s.id]
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrFailedPrecondition, "container must be created")
	}
	return p, nil
}

// getExecProcess returns exec process
func (s *Service) getExecProcess(id string) (rproc.Process, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.processes[id]
	if p == nil {
		return nil, errdefs.ToGRPCf(errdefs.ErrNotFound, "process %s does not exist", id)
	}
	return p, nil
}

func getTopic(ctx context.Context, e interface{}) string {
	switch e.(type) {
	case *eventstypes.TaskCreate:
		return runtime.TaskCreateEventTopic
	case *eventstypes.TaskStart:
		return runtime.TaskStartEventTopic
	case *eventstypes.TaskOOM:
		return runtime.TaskOOMEventTopic
	case *eventstypes.TaskExit:
		return runtime.TaskExitEventTopic
	case *eventstypes.TaskDelete:
		return runtime.TaskDeleteEventTopic
	case *eventstypes.TaskExecAdded:
		return runtime.TaskExecAddedEventTopic
	case *eventstypes.TaskExecStarted:
		return runtime.TaskExecStartedEventTopic
	default:
		logrus.Warnf("no topic for type %#v", e)
	}
	return runtime.TaskUnknownTopic
}

func newInit(ctx context.Context, path, workDir, runtimeRoot, namespace string, config map[string]string, platform rproc.Platform, r *proc.CreateConfig) (*proc.Init, error) {
	var options runctypes.CreateOptions
	if r.Options != nil {
		v, err := typeurl.UnmarshalAny(r.Options)
		if err != nil {
			return nil, err
		}
		options = *v.(*runctypes.CreateOptions)
	}

	spec, err := utils.ReadSpec(r.Bundle)
	if err != nil {
		return nil, errors.Wrap(err, "read oci spec")
	}
	userLog := runsc.FormatLogPath(r.ID, config)
	rootfs := filepath.Join(path, "rootfs")
	runtime := proc.NewRunsc(runtimeRoot, path, namespace, r.Runtime, config)
	p := proc.New(r.ID, runtime, rproc.Stdio{
		Stdin:    r.Stdin,
		Stdout:   r.Stdout,
		Stderr:   r.Stderr,
		Terminal: r.Terminal,
	})
	p.Bundle = r.Bundle
	p.Platform = platform
	p.Rootfs = rootfs
	p.WorkDir = workDir
	p.IoUID = int(options.IoUid)
	p.IoGID = int(options.IoGid)
	p.Sandbox = utils.IsSandbox(spec)
	p.UserLog = userLog
	p.Monitor = shim.Default
	return p, nil
}
