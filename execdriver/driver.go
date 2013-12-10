package execdriver

import (
	"io"
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
}

type Process interface {
	Exec(options *Options) error

	Attach(stdin io.ReadCloser, stdinCloser io.Closer, stdout io.Writer, stderr io.Writer) chan error
}
