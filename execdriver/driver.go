package execdriver

import (
	"errors"
	"fmt"
	"io"
)

var (
	ErrAlreadyRunning      = errors.New("exec: already started")
	ErrDriverNotFound      = errors.New("exec: driver not found")
	ErrProcessStart        = errors.New("The process failed to start. Unkown error")
	ErrProcessStartTimeout = errors.New("The process failed to start due to timed out.")
)

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

type InitFunc func(root string) (Driver, error)

var Drivers = map[string]InitFunc{}

var priorities = []string{
	"lxc",
	"chroot",
}

func New(name, root string) (d Driver, err error) {
	if _, exists := Drivers[name]; !exists {
		return nil, ErrDriverNotFound
	}
	for _, n := range append([]string{name}, priorities...) {
		init, exists := Drivers[n]
		if !exists {
			continue
		}
		d, err = init(root)
		if err != nil {
			fmt.Printf("--------> %s\n", err)
			continue
		}
		break
	}
	return d, err
}
