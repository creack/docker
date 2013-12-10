package execdriver

import (
	"errors"
	"io"
)

var (
	ErrAlreadyRunning = errors.New("exec: already started")
)

type Capabilities struct {
	MemoryLimit            bool
	SwapLimit              bool
	IPv4ForwardingDisabled bool
	AppArmor               bool
}

type Options struct {
	ID          string
	Args        []string
	Hostname    string
	Tty         bool
	Env         []string
	Context     interface{}
	RootFs      string
	SysInitPath string
	MaxMemory   int64
	MaxSwap     int64
	Privileged  bool
	Gateway     string
	User        string
	WorkingDir  string
}

type Driver interface {
	New(root, path string, args []string) Process
}

type Process interface {
	Exec(options *Options) error

	StdinPipe() (io.WriteCloser, error)
	StdoutPipe() (io.ReadCloser, error)
	StderrPipe() (io.ReadCloser, error)

	Attach(stdin io.ReadCloser, stdinCloser io.Closer, stdout io.Writer, stderr io.Writer) chan error
}
