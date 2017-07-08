// Package runtime defines the interface between the GCS and an OCI container
// runtime.
package runtime

import (
	"io"

	oci "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/Microsoft/opengcs/service/gcs/oslayer"
)

// ContainerState gives information about a container created by a Runtime.
type ContainerState struct {
	OCIVersion string
	ID         string
	Pid        int
	BundlePath string
	RootfsPath string
	Status     string
	Created    string
}

// ContainerProcessState gives information about a process created by a
// Runtime.
type ContainerProcessState struct {
	Pid              int
	Command          []string
	CreatedByRuntime bool
	IsZombie         bool
}

// StdioOptions specify how the runtime should handle stdio for the process.
type StdioOptions struct {
	CreateIn  bool
	CreateOut bool
	CreateErr bool
}

// StdioPipes contain the interfaces for reading from and writing to a
// process's stdio.
type StdioPipes struct {
	In  io.WriteCloser
	Out io.ReadCloser
	Err io.ReadCloser
	Pty io.ReadWriteCloser
}

// Close closes all the closers in StdioPipes
func (pipes *StdioPipes) Close() error {
	if pipes == nil {
		return nil
	}
	if pipes.In != nil {
		pipes.In.Close()
		pipes.In = nil
	}
	if pipes.Out != nil {
		pipes.Out.Close()
		pipes.Out = nil
	}
	if pipes.Err != nil {
		pipes.Err.Close()
		pipes.Err = nil
	}
	if pipes.Pty != nil {
		pipes.Pty.Close()
		pipes.Pty = nil
	}
	return nil
}

// Process is an interface to manipulate process state.
type Process interface {
	Wait() (oslayer.ProcessExitState, error)
	Pid() int
	Delete() error
	GetStdioPipes() *StdioPipes
}

// Container is an interface to manipulate container state.
type Container interface {
	Process
	ID() string
	Exists() (bool, error)
	Start() error
	ExecProcess(process oci.Process, stdioOptions StdioOptions) (p Process, err error)
	Kill(signal oslayer.Signal) error
	Pause() error
	Resume() error
	GetState() (*ContainerState, error)
	GetRunningProcesses() ([]ContainerProcessState, error)
	GetAllProcesses() ([]ContainerProcessState, error)
}

// Runtime is the interface defining commands over an OCI container runtime,
// such as runC.
type Runtime interface {
	CreateContainer(id string, bundlePath string, stdioOptions StdioOptions) (c Container, err error)
	ListContainerStates() ([]ContainerState, error)
}
