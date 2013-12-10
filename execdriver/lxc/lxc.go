package lxc

import (
	"encoding/json"
	"github.com/dotcloud/docker/execdriver"
	"github.com/dotcloud/docker/utils"
	"github.com/kr/pty"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"syscall"
)

type Lxc struct {
	root     string
	cmd      *exec.Cmd
	Args     []string
	waitLock chan struct{}

	stdout    *utils.WriteBroadcaster
	stderr    *utils.WriteBroadcaster
	stdin     io.ReadCloser
	stdinPipe io.WriteCloser
	ptyMaster io.Closer
}

func (l *Lxc) monitor() error {
	l.cmd.Wait()
	l.stdout.CloseWriters()
	l.stderr.CloseWriters()
	l.stdin.Close()
	l.stdinPipe.Close()
	l.ptyMaster.Close()
	return nil
}

func (l *Lxc) startPty() error {
	ptyMaster, ptySlave, err := pty.Open()
	if err != nil {
		return err
	}
	l.ptyMaster = ptyMaster
	l.cmd.Stdout = ptySlave
	l.cmd.Stderr = ptySlave

	// Copy the PTYs to our broadcasters
	go func() {
		//	defer container.stdout.CloseWriters()
		//	utils.Debugf("startPty: begin of stdout pipe")
		io.Copy(l.stdout, ptyMaster)
		//	utils.Debugf("startPty: end of stdout pipe")
	}()

	// stdin

	l.cmd.Stdin = ptySlave
	l.cmd.SysProcAttr.Setctty = true
	go func() {
		defer l.stdin.Close()
		utils.Debugf("startPty: begin of stdin pipe")
		io.Copy(ptyMaster, l.stdin)
		utils.Debugf("startPty: end of stdin pipe")
	}()

	if err := l.cmd.Start(); err != nil {
		return err
	}
	ptySlave.Close()
	return nil
}

func (l *Lxc) start() error {
	l.cmd.Stdout = l.stdout
	l.cmd.Stderr = l.stderr

	stdin, err := l.cmd.StdinPipe()
	if err != nil {
		return err
	}
	go func() {
		defer stdin.Close()
		utils.Debugf("start: begin of stdin pipe")
		io.Copy(stdin, l.stdin)
		utils.Debugf("start: end of stdin pipe")
	}()

	return l.cmd.Start()
}

func (l *Lxc) StdinPipe() (io.WriteCloser, error) {
	return l.stdinPipe, nil
}

func (l *Lxc) StdoutPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	l.stdout.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (l *Lxc) StderrPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	l.stderr.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (l *Lxc) Attach(stdin io.ReadCloser, stdinCloser io.Closer, stdout io.Writer, stderr io.Writer) chan error {
	var cStdout, cStderr io.ReadCloser

	var nJobs int
	errors := make(chan error, 3)
	if stdin != nil {
		nJobs += 1
		if cStdin, err := l.StdinPipe(); err != nil {
			errors <- err
		} else {
			go func() {
				utils.Debugf("attach: stdin: begin")
				defer utils.Debugf("attach: stdin: end")
				// No matter what, when stdin is closed (io.Copy unblock), close stdout and stderr

				if cStdout != nil {
					defer cStdout.Close()
				}
				if cStderr != nil {
					defer cStderr.Close()
				}

				if true {
					_, err = utils.CopyEscapable(cStdin, stdin)
				} else {
					_, err = io.Copy(cStdin, stdin)
				}
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					utils.Errorf("attach: stdin: %s", err)
				}
				errors <- err
			}()
		}
	}
	if stdout != nil {
		nJobs += 1
		if p, err := l.StdoutPipe(); err != nil {
			errors <- err
		} else {
			cStdout = p
			go func() {
				utils.Debugf("attach: stdout: begin")
				defer utils.Debugf("attach: stdout: end")
				// If we are in StdinOnce mode, then close stdin
				if stdinCloser != nil {
					defer stdinCloser.Close()
				}
				_, err := io.Copy(stdout, cStdout)
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					utils.Errorf("attach: stdout: %s", err)
				}
				errors <- err
			}()
		}
	} else {
		go func() {
			if stdinCloser != nil {
				defer stdinCloser.Close()
			}
			if cStdout, err := l.StdoutPipe(); err != nil {
				utils.Errorf("attach: stdout pipe: %s", err)
			} else {
				io.Copy(&utils.NopWriter{}, cStdout)
			}
		}()
	}
	if stderr != nil {
		nJobs += 1
		if p, err := l.StderrPipe(); err != nil {
			errors <- err
		} else {
			cStderr = p
			go func() {
				utils.Debugf("attach: stderr: begin")
				defer utils.Debugf("attach: stderr: end")
				// If we are in StdinOnce mode, then close stdin
				if stdinCloser != nil {
					defer stdinCloser.Close()
				}
				_, err := io.Copy(stderr, cStderr)
				if err == io.ErrClosedPipe {
					err = nil
				}
				if err != nil {
					utils.Errorf("attach: stderr: %s", err)
				}
				errors <- err
			}()
		}
	} else {
		go func() {
			if stdinCloser != nil {
				defer stdinCloser.Close()
			}

			if cStderr, err := l.StderrPipe(); err != nil {
				utils.Errorf("attach: stdout pipe: %s", err)
			} else {
				io.Copy(&utils.NopWriter{}, cStderr)
			}
		}()
	}

	return utils.Go(func() error {
		if cStdout != nil {
			defer cStdout.Close()
		}
		if cStderr != nil {
			defer cStderr.Close()
		}
		// FIXME: how to clean up the stdin goroutine without the unwanted side effect
		// of closing the passed stdin? Add an intermediary io.Pipe?
		for i := 0; i < nJobs; i += 1 {
			utils.Debugf("attach: waiting for job %d/%d", i+1, nJobs)
			if err := <-errors; err != nil {
				utils.Errorf("attach: job %d returned error %s, aborting all jobs", i+1, err)
				return err
			}
			utils.Debugf("attach: job %d completed successfully", i+1)
		}
		utils.Debugf("attach: all jobs completed successfully")
		return nil
	})
}

func (l *Lxc) lxcConfigPath() string {
	return path.Join(l.root, "config.lxc")
}

func (l *Lxc) generateLXCConfig(context interface{}) error {
	fo, err := os.Create(l.lxcConfigPath())
	if err != nil {
		return err
	}
	defer fo.Close()
	return LxcTemplateCompiled.Execute(fo, context)
}

func (l *Lxc) Exec(options *execdriver.Options) error {
	l.Args = options.Args

	var lxcStart string = "lxc-start"

	if err := l.generateLXCConfig(options.Context); err != nil {
		return err
	}

	params := []string{
		lxcStart,
		"-n", options.ID,
		"-f", l.lxcConfigPath(),
		"--",
		"/.dockerinit",
	}

	// Setup environment
	env := []string{
		"HOME=/",
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"container=lxc",
		"HOSTNAME=" + options.Hostname,
	}

	if options.Tty {
		env = append(env, "TERM=xterm")
	}

	for _, elem := range options.Env {
		env = append(env, elem)
	}

	if err := l.generateEnvConfig(env); err != nil {
		return err
	}

	// Program
	params = append(params, "--")
	params = append(params, l.Args...)

	l.cmd = exec.Command(params[0], params[1:]...)

	l.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	var err error
	if options.Tty {
		err = l.startPty()
	} else {
		err = l.start()
	}
	if err != nil {
		return err
	}

	// Init the lock
	l.waitLock = make(chan struct{})

	go l.monitor()

	return nil
}

func (l *Lxc) generateEnvConfig(env []string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	ioutil.WriteFile(path.Join(l.root, "config.env"), data, 0600)
	return nil
}

func New(root string, in io.ReadCloser, inp io.WriteCloser, out, errstream *utils.WriteBroadcaster) *Lxc {
	l := &Lxc{}

	l.root = root
	// Attach to stdout and stderr
	l.stderr = utils.NewWriteBroadcaster()
	l.stdout = utils.NewWriteBroadcaster()

	// Attach to stdin
	l.stdin, l.stdinPipe = io.Pipe()
	return l
}
