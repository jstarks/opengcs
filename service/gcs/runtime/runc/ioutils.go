package runc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// createConsoleSocket creates a unix socket in the given process directory and
// returns its path and a listener to it. This socket can then be used to
// receive the container's terminal master file descriptor.
func (r *runcRuntime) createConsoleSocket(processDir string) (listener *net.UnixListener, socketPath string, err error) {
	socketPath = filepath.Join(processDir, "master.sock")
	addr, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return nil, "", errors.Wrapf(err, "failed to resolve unix socket at address %s", socketPath)
	}
	listener, err = net.ListenUnix("unix", addr)
	if err != nil {
		return nil, "", errors.Wrapf(err, "failed to listen on unix socket at address %s", socketPath)
	}
	return listener, socketPath, nil
}

// getMasterFromSocket blocks on the given listener's socket until a message is
// sent, then parses the file descriptor representing the terminal master out
// of the message and returns it as a file.
func (r *runcRuntime) getMasterFromSocket(listener *net.UnixListener) (master *os.File, err error) {
	// Accept the listener's connection.
	conn, err := listener.Accept()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get terminal master file descriptor from socket")
	}
	defer conn.Close()
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return nil, errors.New("connection returned from Accept was not a unix socket")
	}

	const maxNameLen = 4096
	var oobSpace = unix.CmsgSpace(4)

	name := make([]byte, maxNameLen)
	oob := make([]byte, oobSpace)

	// Read a message from the unix socket. This blocks until the message is
	// sent.
	n, oobn, _, _, err := unixConn.ReadMsgUnix(name, oob)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read message from unix socket")
	}
	if n >= maxNameLen || oobn != oobSpace {
		return nil, errors.Errorf("read an invalid number of bytes (n=%d oobn=%d)", n, oobn)
	}

	// Truncate the data returned from the message.
	name = name[:n]
	oob = oob[:oobn]

	// Parse the out-of-band data in the message.
	messages, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse socket control message for oob %v", oob)
	}
	if len(messages) == 0 {
		return nil, errors.New("did not receive any socket control messages")
	}
	if len(messages) > 1 {
		return nil, errors.Errorf("received more than one socket control message: received %d", len(messages))
	}
	message := messages[0]

	// Parse the file descriptor out of the out-of-band data in the message.
	fds, err := unix.ParseUnixRights(&message)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse file descriptors out of message %v", message)
	}
	if len(fds) == 0 {
		return nil, errors.New("did not receive any file descriptors")
	}
	if len(fds) > 1 {
		return nil, errors.Errorf("received more than one file descriptor: received %d", len(fds))
	}
	fd := uintptr(fds[0])

	return os.NewFile(fd, string(name)), nil
}

// NewConsole allocates a new console and returns the File for its master and
// path for its slave.
func NewConsole() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", syscall.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", errors.Wrap(err, "failed to open master pseudoterminal file")
	}
	console, err := ptsname(master)
	if err != nil {
		return nil, "", err
	}
	if err := unlockpt(master); err != nil {
		return nil, "", err
	}
	// TODO: Do we need to keep this chmod call?
	if err := os.Chmod(console, 0600); err != nil {
		return nil, "", errors.Wrap(err, "failed to change permissions on the slave pseudoterminal file")
	}
	if err := os.Chown(console, 0, 0); err != nil {
		return nil, "", errors.Wrap(err, "failed to change ownership on the slave pseudoterminal file")
	}
	return master, console, nil
}

func ioctl(fd uintptr, flag, data uintptr) error {
	if _, _, err := unix.Syscall(unix.SYS_IOCTL, fd, flag, data); err != 0 {
		return err
	}
	return nil
}

// ptsname is a Go wrapper around the ptsname system call. It returns the name
// of the slave pseudoterminal device corresponding to the given master.
func ptsname(f *os.File) (string, error) {
	var n int32
	if err := ioctl(f.Fd(), unix.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		return "", errors.Wrap(err, "ioctl TIOCGPTN failed for ptsname")
	}
	return fmt.Sprintf("/dev/pts/%d", n), nil
}

// unlockpt is a Go wrapper around the unlockpt system call. It unlocks the
// slave pseudoterminal device corresponding to the given master.
func unlockpt(f *os.File) error {
	var u int32
	if err := ioctl(f.Fd(), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&u))); err != nil {
		return errors.Wrap(err, "ioctl TIOCSPTLCK failed for unlockpt")
	}
	return nil
}
