package chroot

import (
	"encoding/json"
	"fmt"
	"github.com/dotcloud/docker/execdriver"
	"github.com/dotcloud/docker/utils"
	"github.com/kr/pty"
	"io"
	"io/ioutil"
	"os/exec"
	"path"
	"syscall"
)

type Chroot struct {
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

func (c *Chroot) monitor() error {
	c.cmd.Wait()
	c.stdout.CloseWriters()
	c.stderr.CloseWriters()
	c.stdin.Close()
	c.stdinPipe.Close()
	c.ptyMaster.Close()
	return nil
}

func (c *Chroot) startPty() error {
	ptyMaster, ptySlave, err := pty.Open()
	if err != nil {
		return err
	}
	c.ptyMaster = ptyMaster
	c.cmd.Stdout = ptySlave
	c.cmd.Stderr = ptySlave

	// Copy the PTYs to our broadcasters
	go func() {
		//	defer container.stdout.CloseWriters()
		//	utils.Debugf("startPty: begin of stdout pipe")
		io.Copy(c.stdout, ptyMaster)
		//	utils.Debugf("startPty: end of stdout pipe")
	}()

	// stdin

	c.cmd.Stdin = ptySlave
	c.cmd.SysProcAttr.Setctty = true
	go func() {
		defer c.stdin.Close()
		utils.Debugf("startPty: begin of stdin pipe")
		io.Copy(ptyMaster, c.stdin)
		utils.Debugf("startPty: end of stdin pipe")
	}()

	if err := c.cmd.Start(); err != nil {
		return err
	}
	ptySlave.Close()
	return nil
}

func (c *Chroot) start() error {
	c.cmd.Stdout = c.stdout
	c.cmd.Stderr = c.stderr

	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	go func() {
		defer stdin.Close()
		utils.Debugf("start: begin of stdin pipe")
		io.Copy(stdin, c.stdin)
		utils.Debugf("start: end of stdin pipe")
	}()

	return c.cmd.Start()
}

func (c *Chroot) StdinPipe() (io.WriteCloser, error) {
	return c.stdinPipe, nil
}

func (c *Chroot) StdoutPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	c.stdout.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (c *Chroot) StderrPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	c.stderr.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (c *Chroot) Attach(stdin io.ReadCloser, stdinCloser io.Closer, stdout io.Writer, stderr io.Writer) chan error {
	var cStdout, cStderr io.ReadCloser

	var nJobs int
	errors := make(chan error, 3)
	if stdin != nil {
		nJobs += 1
		if cStdin, err := c.StdinPipe(); err != nil {
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
		if p, err := c.StdoutPipe(); err != nil {
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
			if cStdout, err := c.StdoutPipe(); err != nil {
				utils.Errorf("attach: stdout pipe: %s", err)
			} else {
				io.Copy(&utils.NopWriter{}, cStdout)
			}
		}()
	}
	if stderr != nil {
		nJobs += 1
		if p, err := c.StderrPipe(); err != nil {
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

			if cStderr, err := c.StderrPipe(); err != nil {
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

func (c *Chroot) lxcConfigPath() string {
	return path.Join(c.root, "config.lxc")
}

func (c *Chroot) Exec(options *execdriver.Options) error {
	c.Args = options.Args

	var lxcStart string = "chroot"

	println("------------>", options.RootFs)

	data, _ := ioutil.ReadFile(options.SysInitPath)

	ioutil.WriteFile(path.Join(options.RootFs, ".dockerinit"), data, 0644)

	params := []string{
		lxcStart,
		options.RootFs,
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

	if err := c.generateEnvConfig(options.RootFs, env); err != nil {
		return err
	}

	// Program
	params = append(params, c.Args...)

	fmt.Printf("---------------> %#v\n", params)
	c.cmd = exec.Command(params[0], params[1:]...)

	c.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	var err error
	if options.Tty {
		err = c.startPty()
	} else {
		err = c.start()
	}
	if err != nil {
		return err
	}

	// Init the lock
	c.waitLock = make(chan struct{})

	go c.monitor()

	return nil
}

func (c *Chroot) generateEnvConfig(rootfs string, env []string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}

	ioutil.WriteFile(path.Join(rootfs, ".dockerenv"), data, 0600)
	return nil
}

func New(root string, in io.ReadCloser, inp io.WriteCloser, out, errstream *utils.WriteBroadcaster) *Chroot {
	c := &Chroot{}

	c.root = root
	// Attach to stdout and stderr
	c.stderr = utils.NewWriteBroadcaster()
	c.stdout = utils.NewWriteBroadcaster()

	// Attach to stdin
	c.stdin, c.stdinPipe = io.Pipe()
	return c
}
