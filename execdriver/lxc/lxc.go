package lxc

import (
	"encoding/json"
	"fmt"
	"github.com/dotcloud/docker/execdriver"
	"github.com/dotcloud/docker/utils"
	"github.com/kr/pty"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"
	"time"
)

const DriverName = "lxc"

type Driver struct {
	root         string
	capabilities *execdriver.Capabilities
}

func init() {
	execdriver.Drivers[DriverName] = NewLxcDriver
}

func NewLxcDriver(root string) (execdriver.Driver, error) {
	if err := linkLxcStart(root); err != nil {
		return nil, err
	}
	return &Driver{
		root:         root,
		capabilities: execdriver.GetCapabilities(),
	}, nil
}

func linkLxcStart(root string) error {
	sourcePath, err := exec.LookPath("lxc-start")
	if err != nil {
		return err
	}
	targetPath := path.Join(root, "lxc-start-unconfined")

	if _, err := os.Stat(targetPath); err != nil && !os.IsNotExist(err) {
		return err
	} else if err == nil {
		if err := os.Remove(targetPath); err != nil {
			return err
		}
	}
	return os.Symlink(sourcePath, targetPath)
}

func rootIsShared() bool {
	if data, err := ioutil.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			cols := strings.Split(line, " ")
			if len(cols) >= 6 && cols[4] == "/" {
				return strings.HasPrefix(cols[6], "shared")
			}
		}
	}

	// No idea, probably safe to assume so
	return true
}

type Lxc struct {
	sync.Mutex

	ID string // We can store the ID as a Lxc instance is tight to a Container instance

	root     string
	cmd      *exec.Cmd
	Path     string
	Args     []string
	waitLock chan struct{}

	stdout    *utils.WriteBroadcaster
	stderr    *utils.WriteBroadcaster
	stdin     io.ReadCloser
	stdinPipe io.WriteCloser
	ptyMaster io.Closer

	driver *Driver

	state execdriver.State
}

func (l *Lxc) GetState() *execdriver.State {
	return &l.state
}

func (l *Lxc) String() string {
	return fmt.Sprintf("%s %s", l.Path, strings.Join(l.Args, " "))
}

func (l *Lxc) GetPty() (*os.File, error) {
	if l.ptyMaster == nil {
		return nil, execdriver.ErrNoTTY
	}
	if pty, ok := l.ptyMaster.(*os.File); ok {
		return pty, nil
	}
	return nil, execdriver.ErrNotATTY
}

// FIXME: replace this with a control socket within dockerinit
func (l *Lxc) waitLxc() error {
	for {
		output, err := exec.Command("lxc-info", "-n", l.ID).CombinedOutput()
		if err != nil {
			return err
		}
		if !strings.Contains(string(output), "RUNNING") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (l *Lxc) monitor() error {
	// Wait for the program to exit

	// If the command does not exist, try to wait via lxc
	// (This probably happens only for ghost containers, i.e. containers that were running when Docker started)
	if l.cmd == nil {
		utils.Debugf("monitor: waiting for container %s using waitLxc", l.ID)
		if err := l.waitLxc(); err != nil {
			utils.Errorf("monitor: while waiting for container %s, waitLxc had a problem: %s", l.ID, err)
		}
	} else {
		utils.Debugf("monitor: waiting for container %s using cmd.Wait", l.ID)
		if err := l.cmd.Wait(); err != nil {
			// Since non-zero exit status and signal terminations will cause err to be non-nil,
			// we have to actually discard it. Still, log it anyway, just in case.
			utils.Debugf("monitor: cmd.Wait reported exit status %s for container %s", err, l.ID)
		}
	}
	utils.Debugf("monitor: container %s finished", l.ID)

	exitCode := -1
	if l.cmd != nil {
		exitCode = l.cmd.ProcessState.Sys().(syscall.WaitStatus).ExitStatus()
	}

	l.stdout.CloseWriters()
	l.stderr.CloseWriters()
	l.stdin.Close()
	l.stdinPipe.Close()
	l.ptyMaster.Close()
	l.cmd = nil

	// 	if container.runtime != nil && container.runtime.srv != nil {
	// 		container.runtime.srv.LogEvent("die", container.ID, container.runtime.repositories.ImageName(container.Image))
	// 	}

	// 	// Cleanup
	// 	container.cleanup()

	// Re-create a brand new stdin pipe once the container exited
	if l.stdin != nil {
		l.stdin, l.stdinPipe = io.Pipe()
	}

	// Report status back
	l.state.SetStopped(exitCode)

	if err := execdriver.SaveState(&l.state, path.Join(l.root, "state")); err != nil {
		return fmt.Errorf("%s: Failed to dump configuration to the disk: %s", l.ID, err)
	}

	// Release the lock
	close(l.waitLock)

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
		defer l.stdout.CloseWriters()
		utils.Debugf("startPty: begin of stdout pipe")
		io.Copy(l.stdout, ptyMaster)
		utils.Debugf("startPty: end of stdout pipe")
	}()

	// stdin
	if l.stdin != nil {
		l.cmd.Stdin = ptySlave
		// FIXME: do we want this all the time or only on TTY mode?
		l.cmd.SysProcAttr.Setctty = true
		go func() {
			defer l.stdin.Close()
			utils.Debugf("startPty: begin of stdin pipe")
			io.Copy(ptyMaster, l.stdin)
			utils.Debugf("startPty: end of stdin pipe")
		}()
	}

	if err := l.cmd.Start(); err != nil {
		return err
	}
	ptySlave.Close()
	return nil
}

func (l *Lxc) start() error {
	l.cmd.Stdout = l.stdout
	l.cmd.Stderr = l.stderr

	if l.stdin != nil {
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
	}
	return l.cmd.Start()
}

// Lxc.StdinPipe returns a WriteCloser which can be used to feed data
// to the standard input of the container's active process.
// Container.StdoutPipe and Container.StderrPipe each return a ReadCloser
// which can be used to retrieve the standard output (and error) generated
// by the container's active process. The output (and error) are actually
// copied and delivered to all StdoutPipe and StderrPipe consumers, using
// a kind of "broadcaster".
func (l *Lxc) StdinPipe() (io.WriteCloser, error) {
	l.stdin, l.stdinPipe = io.Pipe()
	return l.stdinPipe, nil
}

func (l *Lxc) StdoutPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	l.stdout.AddWriter(writer, "")
	return reader, nil
}

func (l *Lxc) StderrPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	l.stderr.AddWriter(writer, "")
	return reader, nil
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

func (l *Lxc) cgroupCheck(opt *execdriver.Options) error {
	// Discard the check if we don't have Capabilities
	if l.driver.capabilities == nil {
		return nil
	}

	// Make sure the config is compatible with the current kernel
	if opt.MaxMemory > 0 && !l.driver.capabilities.MemoryLimit {
		log.Printf("W ARNING: Your kernel does not support memory limit capabilities. Limitation discarded.\n")
		opt.MaxMemory = 0
	}
	if opt.MaxMemory > 0 && !l.driver.capabilities.SwapLimit {
		log.Printf("WARNING: Your kernel does not support swap limit capabilities. Limitation discarded.\n")
		opt.MaxSwap = -1
	}

	if l.driver.capabilities.IPv4ForwardingDisabled {
		log.Printf("WARNING: IPv4 forwarding is disabled. Networking will not work")
	}
	return nil
}

func (l *Lxc) logPath(ID, name string) string {
	return path.Join(l.root, fmt.Sprintf("%s-%s.log", ID, name))
}

func (l *Lxc) waitForStart(ID string) error {
	// We wait for the container to be fully running.
	// Timeout after 5 seconds. In case of broken pipe, just retry.
	// Note: The container can run and finish correctly before
	//       the end of this loop
	for now := time.Now(); time.Since(now) < 5*time.Second; {
		// If the container dies while waiting for it, just return
		// FIXME: Double check this does return false positive (i.e. not yet started)
		if l.state.IsRunning() {
			return nil
		}

		output, err := exec.Command("lxc-info", "-s", "-n", ID).CombinedOutput()
		if err != nil {
			utils.Debugf("Error with lxc-info: %s (%s)", err, output)

			output, err = exec.Command("lxc-info", "-s", "-n", ID).CombinedOutput()
			if err != nil {
				utils.Debugf("Second Error with lxc-info: %s (%s)", err, output)
				return err
			}

		}
		if strings.Contains(string(output), "RUNNING") {
			l.state.SetRunning(l.cmd.Process.Pid)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return execdriver.ErrProcessStartTimeout
}

func (l *Lxc) Exec(options *execdriver.Options) error {
	var lxcStart = "lxc-start"

	if options.Privileged && l.driver.capabilities.AppArmor {
		lxcStart = path.Join(l.driver.root, "lxc-start-unconfined")
	}

	if l.cmd != nil {
		return execdriver.ErrAlreadyRunning
	}

	l.cgroupCheck(options)

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

	if options.Gateway != "" {
		params = append(params, "-g", options.Gateway)
	}

	if options.User != "" {
		params = append(params, "-u", options.User)
	}

	if options.WorkingDir != "" {
		params = append(params, "-w", options.WorkingDir)
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
	params = append(params, "--", l.Path)
	params = append(params, l.Args...)

	if rootIsShared() {
		// lxc-start really needs / to be non-shared, or all kinds of stuff break
		// when lxc-start unmount things and those unmounts propagate to the main
		// mount namespace.
		// What we really want is to clone into a new namespace and then
		// mount / MS_REC|MS_SLAVE, but since we can't really clone or fork
		// without exec in go we have to do this horrible shell hack...
		shellString :=
			"mount --make-rslave /; exec " +
				utils.ShellQuoteArguments(params)

		params = []string{
			"unshare", "-m", "--", "/bin/sh", "-c", shellString,
		}
	}

	l.cmd = exec.Command(params[0], params[1:]...)
	l.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	// LOGS --------------------
	log, err := os.OpenFile(l.logPath(options.ID, "json"), os.O_RDWR|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	l.stdout.AddWriter(log, "stdout")
	l.stderr.AddWriter(log, "stderr")
	// !LOGS -------------------

	starter := l.start
	if options.Tty {
		starter = l.startPty
	}

	if err := starter(); err != nil {
		return err
	}

	// Init the lock
	l.waitLock = make(chan struct{})

	go l.monitor()

	if err := l.waitForStart(options.ID); err != nil {
		return err
	}

	if !l.state.IsRunning() {
		return execdriver.ErrProcessStartTimeout
	}

	if err := execdriver.SaveState(&l.state, path.Join(l.root, "state")); err != nil {
		return err
	}
	return nil
}

func (l *Lxc) generateEnvConfig(env []string) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path.Join(l.root, "config.env"), data, 0600)
}

func (d *Driver) New(root string, Path string, args []string) execdriver.Process {
	l := &Lxc{
		Path:   Path,
		Args:   args,
		root:   root,
		stderr: utils.NewWriteBroadcaster(),
		stdout: utils.NewWriteBroadcaster(),
		driver: d,
	}
	return l
}
