// Package runc defines an implementation of the Runtime interface which uses
// runC as the container runtime.
package runc

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Sirupsen/logrus"
	containerdsys "github.com/docker/containerd/sys"
	oci "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"

	"github.com/Microsoft/opengcs/service/gcs/oslayer"
	"github.com/Microsoft/opengcs/service/gcs/oslayer/realos"
	"github.com/Microsoft/opengcs/service/gcs/runtime"
)

const (
	runcPath          = "/sbin/runc"
	containerFilesDir = "/var/lib/gcsrunc"
	initPidFilename   = "initpid"
)

// runcRuntime is an implementation of the Runtime interface which uses runC as
// the container runtime.
type runcRuntime struct {
}

var _ runtime.Runtime = &runcRuntime{}

type container struct {
	r *runcRuntime
	p *process
}

func (c *container) ID() string {
	return c.p.id
}

func (c *container) Pid() int {
	return c.p.Pid()
}

type process struct {
	r   *runcRuntime
	id  string
	pid int
}

func (p *process) Pid() int {
	return p.pid
}

// NewRuntime instantiates a new runcRuntime struct.
func NewRuntime() (*runcRuntime, error) {
	rtime := &runcRuntime{}
	if err := rtime.initialize(); err != nil {
		return nil, err
	}
	return rtime, nil
}

// initialize sets up any state necessary for the runcRuntime to function.
func (r *runcRuntime) initialize() error {
	exists, err := r.pathExists(containerFilesDir)
	if err != nil {
		return err
	}
	if !exists {
		if err := os.MkdirAll(containerFilesDir, 0700); err != nil {
			return errors.Wrapf(err, "failed making runC container files directory %s", containerFilesDir)
		}
	}
	return nil
}

// CreateContainer creates a container with the given ID and the given
// bundlePath.
// bundlePath should be a path to an OCI bundle containing a config.json file
// and a rootfs for the container.
func (r *runcRuntime) CreateContainer(id string, bundlePath string, stdioOptions runtime.StdioOptions) (c runtime.Container, err error) {
	c, err = r.runCreateCommand(id, bundlePath, stdioOptions)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Start unblocks the container's init process created by the call to
// CreateContainer.
func (c *container) Start() error {
	logPath := c.r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "start", c.p.id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		c.r.cleanupContainer(c.p.id)
		return errors.Wrapf(err, "runc start failed with: %s", out)
	}
	return nil
}

// ExecProcess executes a new process, represented as an OCI process struct,
// inside an already-running container.
func (c *container) ExecProcess(process oci.Process, stdioOptions runtime.StdioOptions) (p runtime.Process, err error) {
	p, err = c.runExecCommand(process, stdioOptions)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Kill sends the specified signal to the container's init process.
func (c *container) Kill(signal oslayer.Signal) error {
	logPath := c.r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "kill", c.p.id, strconv.Itoa(int(signal)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "runc kill failed with: %s", out)
	}
	return nil
}

// Delete deletes any state created for the container by either this
// wrapper or runC itself.
func (c *container) Delete() error {
	logPath := c.r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "delete", c.p.id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "runc delete failed with: %s", out)
	}
	if err := c.r.cleanupContainer(c.p.id); err != nil {
		return err
	}
	return nil
}

// Delete deletes any state created for the process by either this
// wrapper or runC itself.
func (p *process) Delete() error {
	if err := p.r.cleanupProcess(p.id, p.pid); err != nil {
		return err
	}
	return nil
}

// Pause suspends all processes running in the container.
func (c *container) Pause() error {
	logPath := c.r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "pause", c.p.id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "runc pause failed with: %s", out)
	}
	return nil
}

// Resume unsuspends processes running in the container.
func (c *container) Resume() error {
	logPath := c.r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "resume", c.p.id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.Wrapf(err, "runc resume failed with: %s", out)
	}
	return nil
}

// GetState returns information about the given container.
func (c *container) GetState() (*runtime.ContainerState, error) {
	logPath := c.r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "state", c.p.id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrapf(err, "runc state failed with: %s", out)
	}
	var state runtime.ContainerState
	if err := json.Unmarshal(out, &state); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal the state for container %s", c.p.id)
	}
	return &state, nil
}

// Exists returns true if the container exists, false if it doesn't
// exist.
// It should be noted that containers that have stopped but have not been
// deleted are still considered to exist.
func (c *container) Exists() (bool, error) {
	states, err := c.r.ListContainerStates()
	if err != nil {
		return false, err
	}
	// TODO: This is definitely not the most efficient way of doing this. See
	// about improving it in the future.
	for _, state := range states {
		if state.ID == c.p.id {
			return true, nil
		}
	}
	return false, nil
}

// ListContainerStates returns ContainerState structs for all existing
// containers, whether they're running or not.
func (r *runcRuntime) ListContainerStates() ([]runtime.ContainerState, error) {
	logPath := r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "list", "-f", "json")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrapf(err, "runc list failed with: %s", out)
	}
	var states []runtime.ContainerState
	if err := json.Unmarshal(out, &states); err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal the states for the container list")
	}
	return states, nil
}

// GetRunningProcesses gets only the running processes associated with
// the given container. This excludes zombie processes.
func (c *container) GetRunningProcesses() ([]runtime.ContainerProcessState, error) {
	pids, err := c.r.getRunningPids(c.p.id)
	if err != nil {
		return nil, err
	}

	pidMap := map[int]*runtime.ContainerProcessState{}
	// Initialize all processes with a pid and command, and mark correctly that
	// none of them are zombies. Default CreatedByRuntime to false.
	for _, pid := range pids {
		command, err := c.r.getProcessCommand(pid)
		if err != nil {
			return nil, err
		}
		pidMap[pid] = &runtime.ContainerProcessState{Pid: pid, Command: command, CreatedByRuntime: false, IsZombie: false}
	}

	// For each process state directory which corresponds to a running pid, set
	// that the process was created by the Runtime.
	processDirs, err := ioutil.ReadDir(filepath.Join(containerFilesDir, c.p.id))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read the contents of container directory %s", filepath.Join(containerFilesDir, c.p.id))
	}
	for _, processDir := range processDirs {
		if processDir.Name() != initPidFilename {
			pid, err := strconv.Atoi(processDir.Name())
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse string \"%s\" as pid", processDir.Name())
			}
			if _, ok := pidMap[pid]; ok {
				pidMap[pid].CreatedByRuntime = true
			}
		}
	}

	return c.r.pidMapToProcessStates(pidMap), nil
}

// GetAllProcesses gets all processes associated with the given
// container, including both running and zombie processes.
func (c *container) GetAllProcesses() ([]runtime.ContainerProcessState, error) {
	runningPids, err := c.r.getRunningPids(c.p.id)
	if err != nil {
		return nil, err
	}

	pidMap := map[int]*runtime.ContainerProcessState{}
	// Initialize all processes with a pid and command, leaving
	// CreatedByRuntime and IsZombie at the default value of false.
	for _, pid := range runningPids {
		command, err := c.r.getProcessCommand(pid)
		if err != nil {
			return nil, err
		}
		pidMap[pid] = &runtime.ContainerProcessState{Pid: pid, Command: command, CreatedByRuntime: false, IsZombie: false}
	}

	processDirs, err := ioutil.ReadDir(filepath.Join(containerFilesDir, c.p.id))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read the contents of container directory %s", filepath.Join(containerFilesDir, c.p.id))
	}
	// Loop over every process state directory. Since these processes have
	// process state directories, CreatedByRuntime will be true for all of
	// them.
	for _, processDir := range processDirs {
		if processDir.Name() != initPidFilename {
			pid, err := strconv.Atoi(processDir.Name())
			if err != nil {
				return nil, errors.Wrapf(err, "failed to parse string \"%s\" into pid", processDir.Name())
			}
			if c.r.processExists(pid) {
				// If the process exists in /proc and is in the pidMap, it must be
				// a running non-zombie.
				if _, ok := pidMap[pid]; ok {
					pidMap[pid].CreatedByRuntime = true
				} else {
					// Otherwise, since it's in /proc but not running, it must be a
					// zombie.
					command, err := c.r.getProcessCommand(pid)
					if err != nil {
						return nil, err
					}
					pidMap[pid] = &runtime.ContainerProcessState{Pid: pid, Command: command, CreatedByRuntime: true, IsZombie: true}
				}
			}
		}
	}

	return c.r.pidMapToProcessStates(pidMap), nil
}

// getRunningPids gets the pids of all processes which runC recognizes as
// running.
func (r *runcRuntime) getRunningPids(id string) ([]int, error) {
	logPath := r.getLogPath()
	cmd := exec.Command(runcPath, "--log", logPath, "ps", "-f", "json", id)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrapf(err, "runc ps failed with: %s", out)
	}
	var pids []int
	if err := json.Unmarshal(out, &pids); err != nil {
		return nil, errors.Wrapf(err, "failed to unmarshal pids for container %s", id)
	}
	return pids, nil
}

// getProcessCommand gets the command line command and arguments for the
// process with the given pid.
func (r *runcRuntime) getProcessCommand(pid int) ([]string, error) {
	// Get the contents of the process's cmdline file.
	// This file is formatted with a null character after every argument.
	// e.g. "ping\0google.com\0"
	data, err := ioutil.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to read cmdline file for process %d", pid)
	}
	// Get rid of the \0 character at end.
	cmdString := strings.TrimSuffix(string(data), "\x00")
	return strings.Split(cmdString, "\x00"), nil
}

// pidMapToProcessStates is a helper function which converts a map from pid to
// ContainerProcessState to a slice of ContainerProcessStates.
func (r *runcRuntime) pidMapToProcessStates(pidMap map[int]*runtime.ContainerProcessState) []runtime.ContainerProcessState {
	processStates := make([]runtime.ContainerProcessState, len(pidMap))
	i := 0
	for _, processState := range pidMap {
		processStates[i] = *processState
		i++
	}
	return processStates
}

// waitOnProcess waits for the process to exit, and returns its
// oslayer.ProcessExitState containing exit information.
// TODO: We might want to give more options for this, such as specifying
// WNOHANG.
func (r *runcRuntime) waitOnProcess(pid int) (oslayer.ProcessExitState, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find process %d", pid)
	}
	state, err := process.Wait()
	if err != nil {
		return nil, errors.Wrapf(err, "failed waiting on process %d", pid)
	}
	return realos.NewProcessExitState(state), nil
}

func (p *process) Wait() (oslayer.ProcessExitState, error) {
	return p.r.waitOnProcess(p.pid)
}

// Wait waits on every non-init process in the container, and then
// performs a final wait on the init process.
// The oslayer.ProcessExitState returned is the state acquired from waiting on
// the init process.
func (c *container) Wait() (oslayer.ProcessExitState, error) {
	processes, err := c.GetAllProcesses()
	if err != nil {
		return nil, err
	}
	for _, process := range processes {
		// Only wait on non-init processes that were created with exec.
		if process.Pid != c.p.pid && process.CreatedByRuntime {
			// TODO: Should we check for errors here?
			c.r.waitOnProcess(process.Pid)
		}
	}
	state, err := c.p.Wait()
	if err != nil {
		return nil, err
	}
	return state, nil
}

// runCreateCommand sets up the arguments for calling runc create.
func (r *runcRuntime) runCreateCommand(id string, bundlePath string, stdioOptions runtime.StdioOptions) (c runtime.Container, err error) {
	if err := r.makeContainerDir(id); err != nil {
		return nil, err
	}
	// Create a temporary random directory to store the process's files.
	tempProcessDir, err := ioutil.TempDir(containerFilesDir, id)
	if err != nil {
		return nil, err
	}

	hasTerminal, err := r.hasTerminal(bundlePath)
	if err != nil {
		return nil, err
	}
	args := []string{"create", "-b", bundlePath, "--no-pivot"}
	p, err := r.startProcess(id, tempProcessDir, hasTerminal, stdioOptions, args...)
	if err != nil {
		return nil, err
	}

	// Write pid to initpid file for container.
	containerDir := r.getContainerDir(id)
	if err := ioutil.WriteFile(filepath.Join(containerDir, initPidFilename), []byte(strconv.Itoa(p.pid)), 0777); err != nil {
		return nil, err
	}

	c = &container{r: r, p: p}
	return c, nil
}

// hasTerminal looks at the config.json in the bundlePath, and determines
// whether its process's terminal value is true or false.
func (r *runcRuntime) hasTerminal(bundlePath string) (bool, error) {
	configFile, err := os.Open(filepath.Join(bundlePath, "config.json"))
	if err != nil {
		return false, errors.Wrapf(err, "failed to open config file %s", filepath.Join(bundlePath, "config.json"))
	}
	var config oci.Spec
	if err := json.NewDecoder(configFile).Decode(&config); err != nil {
		return false, errors.Wrap(err, "failed to decode config file as JSON")
	}
	return config.Process.Terminal, nil
}

// runExecCommand sets up the arguments for calling runc exec.
func (c *container) runExecCommand(processDef oci.Process, stdioOptions runtime.StdioOptions) (p runtime.Process, err error) {
	// Create a temporary random directory to store the process's files.
	tempProcessDir, err := ioutil.TempDir(containerFilesDir, c.p.id)
	if err != nil {
		return nil, err
	}

	f, err := os.Create(filepath.Join(tempProcessDir, "process.json"))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create process.json file at %s", filepath.Join(tempProcessDir, "process.json"))
	}
	if err := json.NewEncoder(f).Encode(processDef); err != nil {
		return nil, errors.Wrap(err, "failed to decode JSON from process.json file")
	}

	args := []string{"exec"}
	args = append(args, "-d", "--process", filepath.Join(tempProcessDir, "process.json"))
	return c.r.startProcess(c.p.id, tempProcessDir, processDef.Terminal, stdioOptions, args...)
}

// startProcess performs the operations necessary to start a container process
// and properly handle its stdio.
// This function is used by both CreateContainer and ExecProcess.
func (r *runcRuntime) startProcess(id string, tempProcessDir string, hasTerminal bool, stdioOptions runtime.StdioOptions, initialArgs ...string) (p *process, err error) {
	args := initialArgs

	if err := containerdsys.SetSubreaper(1); err != nil {
		return nil, errors.Wrapf(err, "failed to set process as subreaper for process in container %s", id)
	}

	logPath := r.getLogPath()
	args = append([]string{"--log", logPath}, args...)

	args = append(args, "--pid-file", filepath.Join(tempProcessDir, "pid"))

	var cmdStdin *os.File
	var cmdStdout *os.File
	var cmdStderr *os.File
	if hasTerminal {
		sockListener, consoleSockPath, err := r.createConsoleSocket(tempProcessDir)
		if err != nil {
			return nil, err
		}
		args = append(args, "--console-socket", consoleSockPath)
		// setupIOForTerminal blocks, so it needs to run in a separate go
		// routine.
		go func() {
			if err := r.setupIOForTerminal(tempProcessDir, stdioOptions, sockListener); err != nil {
				logrus.Error(err)
			}
		}()

	} else {
		ioSet, err := r.setupIOWithoutTerminal(id, tempProcessDir, stdioOptions)
		if err != nil {
			return nil, err
		}
		cmdStdin = ioSet.InR
		cmdStdout = ioSet.OutW
		cmdStderr = ioSet.ErrW
	}
	args = append(args, id)

	cmd := exec.Command(runcPath, args...)
	cmd.Stdin = cmdStdin
	cmd.Stdout = cmdStdout
	cmd.Stderr = cmdStderr
	if err := cmd.Start(); err != nil {
		return nil, errors.Wrapf(err, "failed to start runc create/exec call for container %s", id)
	}
	if err := cmd.Wait(); err != nil {
		return nil, errors.Wrapf(err, "failed to wait on runc create/exec call for container %s", id)
	}

	// Rename the process's directory to its pid.
	pid, err := r.readPidFile(filepath.Join(tempProcessDir, "pid"))
	if err != nil {
		return nil, err
	}
	if err := os.Rename(tempProcessDir, filepath.Join(r.getContainerDir(id), strconv.Itoa(pid))); err != nil {
		return nil, err
	}
	return &process{r: r, id: id, pid: pid}, nil
}
