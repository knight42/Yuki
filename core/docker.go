package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"sync"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/ustclug/Yuki/common"
	"github.com/ustclug/Yuki/events"
)

// Container provides the ID and labels of a container.
type Container struct {
	ID     string
	Labels map[string]string
}

// SyncOptions provides params to the Sync function.
type SyncOptions struct {
	Name          string
	LogDir        string
	DefaultOwner  string
	DefaultBindIP string
	NamePrefix    string
	Debug         bool
	MountDir      bool
	// FIXME: Not sure whether we should add this param. If a container timed out and got removed, the problem may be hidden.
	Timeout time.Duration
}

// LogsOptions provides params to the GetContainerLogs function.
type LogsOptions struct {
	ID          string
	Stream      io.Writer
	Tail        string
	Follow      bool
	CloseNotify <-chan bool
}

// GetContainerLogs gets all stdout and stderr logs from the given container.
func (c *Core) GetContainerLogs(opts LogsOptions) error {
	finished := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(c.ctx)

	go func() {
		select {
		case <-opts.CloseNotify:
		case <-finished:
		}
		cancel()
	}()

	err := c.Docker.Logs(docker.LogsOptions{
		Stdout:       true,
		Stderr:       true,
		Context:      ctx,
		Container:    opts.ID,
		OutputStream: opts.Stream,
		ErrorStream:  opts.Stream,
		Tail:         opts.Tail,
		Follow:       opts.Follow,
	})
	close(finished)
	if err != context.Canceled {
		return err
	}
	return nil
}

// UpgradeImages pulls all in use Docker images.
func (c *Core) UpgradeImages() {
	var images []string
	err := c.repoColl.Find(nil).Distinct("image", &images)
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	wg.Add(len(images))
	for _, i := range images {
		go func(i string) {
			defer wg.Done()
			c.PullImage(i)
		}(i)
	}
	wg.Wait()
}

// CleanImages remove unused Docker images with `ustcmirror.images` label.
func (c *Core) CleanImages() {
	ctx, cancel := context.WithTimeout(c.ctx, time.Second*10)
	defer cancel()
	imgs, err := c.Docker.ListImages(docker.ListImagesOptions{
		All:     true,
		Context: ctx,
		Filters: map[string][]string{
			"dangling": {"true"},
			"label":    {"org.ustcmirror.images=true"},
		},
	})
	if err != nil {
		return
	}
	for _, i := range imgs {
		c.Docker.RemoveImage(i.ID)
	}
}

// PullImage pulls an image from remote registry.
func (c *Core) PullImage(img string) error {
	repo, tag := docker.ParseRepositoryTag(img)
	return c.Docker.PullImage(docker.PullImageOptions{
		Tag:               tag,
		Repository:        repo,
		InactivityTimeout: time.Second * 10,
	}, docker.AuthConfiguration{})
}

// StopContainer stops the given container.
func (c *Core) StopContainer(id string) error {
	return c.Docker.StopContainer(id, 10)
}

// RemoveContainer removes the given container.
func (c *Core) RemoveContainer(id string) error {
	ctx, cancel := context.WithTimeout(c.ctx, time.Second*20)
	defer cancel()
	opts := docker.RemoveContainerOptions{
		Context:       ctx,
		Force:         true,
		ID:            id,
		RemoveVolumes: true,
	}
	return c.Docker.RemoveContainer(opts)
}

// WaitRunningContainers waits for all syncing containers to stop and remove them.
func (c *Core) WaitRunningContainers() {
	opts := docker.ListContainersOptions{
		All: true,
		Filters: map[string][]string{
			"label":  {"org.ustcmirror.syncing=true"},
			"status": {"running"},
		},
	}
	cts, err := c.Docker.ListContainers(opts)
	if err != nil {
		return
	}
	for _, ct := range cts {
		go c.WaitForSync(Container{
			ID:     ct.ID,
			Labels: ct.Labels,
		})
	}
}

// CleanDeadContainers removes containers which status are `created`, `exited` or `dead`.
func (c *Core) CleanDeadContainers() {
	ctx, cancel := context.WithTimeout(c.ctx, time.Second*10)
	defer cancel()
	opts := docker.ListContainersOptions{
		Context: ctx,
		All:     true,
		Filters: map[string][]string{
			"label":  {"org.ustcmirror.syncing=true"},
			"status": {"created", "exited", "dead"},
		},
	}
	cts, err := c.Docker.ListContainers(opts)
	if err != nil {
		return
	}
	for _, ct := range cts {
		c.RemoveContainer(ct.ID)
	}
}

// Sync creates and starts a predefined container to sync local repository.
func (c *Core) Sync(opts SyncOptions) (*Container, error) {
	r, err := c.GetRepository(opts.Name)
	if err != nil {
		return nil, fmt.Errorf("cannot find <%s> in the DB", opts.Name)
	}

	envs := docker.Env{}
	for k, v := range r.Envs {
		envs.Set(k, v)
	}
	if r.BindIP == "" {
		r.BindIP = opts.DefaultBindIP
	}
	if r.User == "" {
		r.User = opts.DefaultOwner
	}
	envs.Set("REPO", r.Name)
	envs.Set("OWNER", r.User)
	envs.Set("BIND_ADDRESS", r.BindIP)
	envs.SetInt("RETRY", r.Retry)
	envs.SetInt("LOG_ROTATE_CYCLE", r.LogRotCycle)
	if opts.Debug {
		envs.Set("DEBUG", "true")
	} else {
		envs.Set("DEBUG", "false")
	}

	binds := []string{}
	for k, v := range r.Volumes {
		binds = append(binds, fmt.Sprintf("%s:%s", k, v))
	}

	if opts.MountDir {
		logdir := path.Join(opts.LogDir, opts.Name)
		if err = os.MkdirAll(logdir, os.ModePerm); err != nil {
			return nil, fmt.Errorf("not a directory: %s", logdir)
		}
		if !common.DirExists(r.StorageDir) {
			return nil, fmt.Errorf("not a directory: %s", r.StorageDir)
		}
		binds = append(binds, fmt.Sprintf("%s:/data/", r.StorageDir), fmt.Sprintf("%s:/log/", logdir))
	}
	labels := M{
		"org.ustcmirror.name":        r.Name,
		"org.ustcmirror.syncing":     "true",
		"org.ustcmirror.storage-dir": r.StorageDir,
	}
	createOpts := docker.CreateContainerOptions{
		Name: opts.NamePrefix + opts.Name,
		Config: &docker.Config{
			Image:     r.Image,
			OpenStdin: true,
			Env:       envs,
			Labels:    labels,
		},
		HostConfig: &docker.HostConfig{
			Binds:       binds,
			NetworkMode: "host",
		},
	}

	var ct *docker.Container
	ct, err = c.Docker.CreateContainer(createOpts)
	if err != nil {
		if err == docker.ErrNoSuchImage {
			if err = c.PullImage(r.Image); err == nil {
				ct, err = c.Docker.CreateContainer(createOpts)
			}
		}
		if err != nil {
			return nil, err
		}
	}

	if err = c.Docker.StartContainer(ct.ID, nil); err != nil {
		return nil, err
	}

	return &Container{ct.ID, labels}, nil
}

// WaitForSync emits `SyncStart` event at first, then blocks until the container stops and emits the `SyncEnd` event.
func (c *Core) WaitForSync(ct Container) error {
	events.Emit(events.Payload{
		Evt: events.SyncStart,
		Attrs: events.M{
			"Name": ct.Labels["org.ustcmirror.name"],
		},
	})

	code, err := c.Docker.WaitContainer(ct.ID)
	if err != nil {
		return err
	}

	name, ok := ct.Labels["org.ustcmirror.name"]
	if !ok {
		return fmt.Errorf("missing label: org.ustcmirror.name")
	}
	dir, ok := ct.Labels["org.ustcmirror.storage-dir"]
	if !ok {
		return fmt.Errorf("missing label: org.ustcmirror.storage-dir")
	}

	events.Emit(events.Payload{
		Evt: events.SyncEnd,
		Attrs: events.M{
			"ID":       ct.ID,
			"Name":     name,
			"Dir":      dir,
			"ExitCode": code,
		},
	})

	return nil
}
