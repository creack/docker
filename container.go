package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dotcloud/docker/archive"
	"github.com/dotcloud/docker/execdriver"
	"github.com/dotcloud/docker/graphdriver"
	"github.com/dotcloud/docker/term"
	"github.com/dotcloud/docker/utils"
	"io"
	"io/ioutil"
	"log"
	_ "net"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	ErrNotATTY = errors.New("The PTY is not a file")
	ErrNoTTY   = errors.New("No PTY found")
)

type Container struct {
	sync.Mutex
	root   string // Path to the "home" of the container, including metadata.
	rootfs string // Path to the root filesystem of the container.

	ID string

	Created time.Time

	process execdriver.Process

	Config *Config
	//	State  State
	Image string

	network         *NetworkInterface
	NetworkSettings *NetworkSettings

	SysInitPath    string
	ResolvConfPath string
	HostnamePath   string
	HostsPath      string
	Name           string
	Driver         string

	stdout    *utils.WriteBroadcaster
	stderr    *utils.WriteBroadcaster
	stdin     io.ReadCloser
	stdinPipe io.WriteCloser
	ptyMaster io.Closer

	runtime *Runtime

	waitLock chan struct{}
	Volumes  map[string]string
	// Store rw/ro in a separate structure to preserve reverse-compatibility on-disk.
	// Easier than migrating older container configs :)
	VolumesRW  map[string]bool
	hostConfig *HostConfig

	activeLinks map[string]*Link
}

// Note: the Config structure should hold only portable information about the container.
// Here, "portable" means "independent from the host we are running on".
// Non-portable information *should* appear in HostConfig.
type Config struct {
	Hostname        string
	Domainname      string
	User            string
	Memory          int64 // Memory limit (in bytes)
	MemorySwap      int64 // Total memory usage (memory + swap); set `-1' to disable swap
	CpuShares       int64 // CPU shares (relative weight vs. other containers)
	AttachStdin     bool
	AttachStdout    bool
	AttachStderr    bool
	PortSpecs       []string // Deprecated - Can be in the format of 8080/tcp
	ExposedPorts    map[Port]struct{}
	Tty             bool // Attach standard streams to a tty, including stdin if it is not closed.
	OpenStdin       bool // Open stdin
	StdinOnce       bool // If true, close stdin after the 1 attached client disconnects.
	Env             []string
	Cmd             []string
	Dns             []string
	Image           string // Name of the image as it was passed by the operator (eg. could be symbolic)
	Volumes         map[string]struct{}
	VolumesFrom     string
	WorkingDir      string
	Entrypoint      []string
	NetworkDisabled bool
}

type HostConfig struct {
	Binds           []string
	ContainerIDFile string
	LxcConf         []KeyValuePair
	Privileged      bool
	PortBindings    map[Port][]PortBinding
	Links           []string
	PublishAllPorts bool
}

type BindMap struct {
	SrcPath string
	DstPath string
	Mode    string
}

var (
	ErrInvalidWorikingDirectory = errors.New("The working directory is invalid. It needs to be an absolute path.")
	ErrConflictAttachDetach     = errors.New("Conflicting options: -a and -d")
	ErrConflictDetachAutoRemove = errors.New("Conflicting options: -rm and -d")
)

type KeyValuePair struct {
	Key   string
	Value string
}

type PortBinding struct {
	HostIp   string
	HostPort string
}

// 80/tcp
type Port string

func (p Port) Proto() string {
	parts := strings.Split(string(p), "/")
	if len(parts) == 1 {
		return "tcp"
	}
	return parts[1]
}

func (p Port) Port() string {
	return strings.Split(string(p), "/")[0]
}

func (p Port) Int() int {
	i, err := parsePort(p.Port())
	if err != nil {
		panic(err)
	}
	return i
}

func NewPort(proto, port string) Port {
	return Port(fmt.Sprintf("%s/%s", port, proto))
}

type PortMapping map[string]string // Deprecated

type NetworkSettings struct {
	IPAddress   string
	IPPrefixLen int
	Gateway     string
	Bridge      string
	PortMapping map[string]PortMapping // Deprecated
	Ports       map[Port][]PortBinding
}

func (settings *NetworkSettings) PortMappingAPI() []APIPort {
	var mapping []APIPort
	for port, bindings := range settings.Ports {
		p, _ := parsePort(port.Port())
		if len(bindings) == 0 {
			mapping = append(mapping, APIPort{
				PublicPort: int64(p),
				Type:       port.Proto(),
			})
			continue
		}
		for _, binding := range bindings {
			p, _ := parsePort(port.Port())
			h, _ := parsePort(binding.HostPort)
			mapping = append(mapping, APIPort{
				PrivatePort: int64(p),
				PublicPort:  int64(h),
				Type:        port.Proto(),
				IP:          binding.HostIp,
			})
		}
	}
	return mapping
}

// Inject the io.Reader at the given path. Note: do not close the reader
func (container *Container) Inject(file io.Reader, pth string) error {
	return nil
	// if err := container.EnsureMounted(); err != nil {
	// 	return fmt.Errorf("inject: error mounting container %s: %s", container.ID, err)
	// }

	// // Return error if path exists
	// destPath := path.Join(container.RootfsPath(), pth)
	// if _, err := os.Stat(destPath); err == nil {
	// 	// Since err is nil, the path could be stat'd and it exists
	// 	return fmt.Errorf("%s exists", pth)
	// } else if !os.IsNotExist(err) {
	// 	// Expect err might be that the file doesn't exist, so
	// 	// if it's some other error, return that.

	// 	return err
	// }

	// // Make sure the directory exists
	// if err := os.MkdirAll(path.Join(container.RootfsPath(), path.Dir(pth)), 0755); err != nil {
	// 	return err
	// }

	// dest, err := os.Create(destPath)
	// if err != nil {
	// 	return err
	// }
	// defer dest.Close()

	// if _, err := io.Copy(dest, file); err != nil {
	// 	return err
	// }
	// return nil
}

// func (container *Container) Cmd() *exec.Cmd {
// 	return container.cmd
// }

func (container *Container) When() time.Time {
	return container.Created
}

func (container *Container) FromDisk() error {
	data, err := ioutil.ReadFile(container.jsonPath())
	if err != nil {
		return err
	}
	// Load container settings
	// udp broke compat of docker.PortMapping, but it's not used when loading a container, we can skip it
	if err := json.Unmarshal(data, container); err != nil && !strings.Contains(err.Error(), "docker.PortMapping") {
		return err
	}
	return container.readHostConfig()
}

func (container *Container) ToDisk() (err error) {
	data, err := json.Marshal(container)
	if err != nil {
		return
	}
	err = ioutil.WriteFile(container.jsonPath(), data, 0666)
	if err != nil {
		return
	}
	return container.writeHostConfig()
}

func (container *Container) readHostConfig() error {
	container.hostConfig = &HostConfig{}
	// If the hostconfig file does not exist, do not read it.
	// (We still have to initialize container.hostConfig,
	// but that's OK, since we just did that above.)
	_, err := os.Stat(container.hostConfigPath())
	if os.IsNotExist(err) {
		return nil
	}
	data, err := ioutil.ReadFile(container.hostConfigPath())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, container.hostConfig)
}

func (container *Container) writeHostConfig() (err error) {
	data, err := json.Marshal(container.hostConfig)
	if err != nil {
		return
	}
	return ioutil.WriteFile(container.hostConfigPath(), data, 0666)
}

// DEPRECATED
func (container *Container) Attach(stdin io.ReadCloser, stdinCloser io.Closer, stdout io.Writer, stderr io.Writer) chan error {
	return container.process.Attach(stdin, stdinCloser, stdout, stderr)
}

func (container *Container) Start() (err error) {
	container.Lock()
	defer container.Unlock()

	defer func() {
		if err != nil {
			container.cleanup()
		}
	}()

	if container.runtime.networkManager.disabled {
		container.Config.NetworkDisabled = true
		container.buildHostnameAndHostsFiles("127.0.1.1")
	} else {
		if err := container.allocateNetwork(); err != nil {
			return err
		}
		container.buildHostnameAndHostsFiles(container.NetworkSettings.IPAddress)
	}

	if container.Volumes == nil || len(container.Volumes) == 0 {
		container.Volumes = make(map[string]string)
		container.VolumesRW = make(map[string]bool)
	}

	// Apply volumes from another container if requested
	if err := container.applyExternalVolumes(); err != nil {
		return err
	}

	if err := container.createVolumes(); err != nil {
		return err
	}

	var gateway string
	if !container.Config.NetworkDisabled {
		gateway = container.network.Gateway.String()
	}

	opts := &execdriver.Options{
		ID:          container.ID,
		Context:     container,
		Hostname:    container.Config.Hostname,
		Tty:         container.Config.Tty,
		Env:         container.Config.Env,
		RootFs:      container.rootfs,
		SysInitPath: container.SysInitPath,
		MaxMemory:   container.Config.Memory,
		Privileged:  container.hostConfig.Privileged,
		Gateway:     gateway,
		User:        container.Config.User,
	}

	// Init any links between the parent and children
	runtime := container.runtime
	children, err := runtime.Children(container.Name)
	if err != nil {
		return err
	}

	if len(children) > 0 {
		container.activeLinks = make(map[string]*Link, len(children))

		// If we encounter an error make sure that we rollback any network
		// config and ip table changes
		rollback := func() {
			for _, link := range container.activeLinks {
				link.Disable()
			}
			container.activeLinks = nil
		}

		for p, child := range children {
			link, err := NewLink(container, child, p, runtime.networkManager.bridgeIface)
			if err != nil {
				rollback()
				return err
			}

			container.activeLinks[link.Alias()] = link
			if err := link.Enable(); err != nil {
				rollback()
				return err
			}

			for _, envVar := range link.ToEnv() {
				opts.Env = append(opts.Env, envVar)
			}
		}
	}

	if container.Config.WorkingDir != "" {
		workingDir := path.Clean(container.Config.WorkingDir)

		if err := os.MkdirAll(path.Join(container.RootfsPath(), workingDir), 0755); err != nil {
			return nil
		}
	}

	return container.process.Exec(opts)
}

func (container *Container) getBindMap() (map[string]BindMap, error) {
	// Create the requested bind mounts
	binds := make(map[string]BindMap)
	// Define illegal container destinations
	illegalDsts := []string{"/", "."}

	for _, bind := range container.hostConfig.Binds {
		// FIXME: factorize bind parsing in parseBind
		var src, dst, mode string
		arr := strings.Split(bind, ":")
		if len(arr) == 2 {
			src = arr[0]
			dst = arr[1]
			mode = "rw"
		} else if len(arr) == 3 {
			src = arr[0]
			dst = arr[1]
			mode = arr[2]
		} else {
			return nil, fmt.Errorf("Invalid bind specification: %s", bind)
		}

		// Bail if trying to mount to an illegal destination
		for _, illegal := range illegalDsts {
			if dst == illegal {
				return nil, fmt.Errorf("Illegal bind destination: %s", dst)
			}
		}

		bindMap := BindMap{
			SrcPath: src,
			DstPath: dst,
			Mode:    mode,
		}
		binds[path.Clean(dst)] = bindMap
	}
	return binds, nil
}

func (container *Container) createVolumes() error {
	binds, err := container.getBindMap()
	if err != nil {
		return err
	}
	volumesDriver := container.runtime.volumes.driver
	// Create the requested volumes if they don't exist
	for volPath := range container.Config.Volumes {
		volPath = path.Clean(volPath)
		volIsDir := true
		// Skip existing volumes
		if _, exists := container.Volumes[volPath]; exists {
			continue
		}
		var srcPath string
		var isBindMount bool
		srcRW := false
		// If an external bind is defined for this volume, use that as a source
		if bindMap, exists := binds[volPath]; exists {
			isBindMount = true
			srcPath = bindMap.SrcPath
			if strings.ToLower(bindMap.Mode) == "rw" {
				srcRW = true
			}
			if stat, err := os.Lstat(bindMap.SrcPath); err != nil {
				return err
			} else {
				volIsDir = stat.IsDir()
			}
			// Otherwise create an directory in $ROOT/volumes/ and use that
		} else {

			// Do not pass a container as the parameter for the volume creation.
			// The graph driver using the container's information ( Image ) to
			// create the parent.
			c, err := container.runtime.volumes.Create(nil, nil, "", "", nil)
			if err != nil {
				return err
			}
			srcPath, err = volumesDriver.Get(c.ID)
			if err != nil {
				return fmt.Errorf("Driver %s failed to get volume rootfs %s: %s", volumesDriver, c.ID, err)
			}
			srcRW = true // RW by default
		}
		container.Volumes[volPath] = srcPath
		container.VolumesRW[volPath] = srcRW
		// Create the mountpoint
		rootVolPath := path.Join(container.RootfsPath(), volPath)
		if volIsDir {
			if err := os.MkdirAll(rootVolPath, 0755); err != nil {
				return err
			}
		}

		volPath = path.Join(container.RootfsPath(), volPath)
		if _, err := os.Stat(volPath); err != nil {
			if os.IsNotExist(err) {
				if volIsDir {
					if err := os.MkdirAll(volPath, 0755); err != nil {
						return err
					}
				} else {
					if err := os.MkdirAll(path.Dir(volPath), 0755); err != nil {
						return err
					}
					if f, err := os.OpenFile(volPath, os.O_CREATE, 0755); err != nil {
						return err
					} else {
						f.Close()
					}
				}
			}
		}

		// Do not copy or change permissions if we are mounting from the host
		if srcRW && !isBindMount {
			volList, err := ioutil.ReadDir(rootVolPath)
			if err != nil {
				return err
			}
			if len(volList) > 0 {
				srcList, err := ioutil.ReadDir(srcPath)
				if err != nil {
					return err
				}
				if len(srcList) == 0 {
					// If the source volume is empty copy files from the root into the volume
					if err := archive.CopyWithTar(rootVolPath, srcPath); err != nil {
						return err
					}

					var stat syscall.Stat_t
					if err := syscall.Stat(rootVolPath, &stat); err != nil {
						return err
					}
					var srcStat syscall.Stat_t
					if err := syscall.Stat(srcPath, &srcStat); err != nil {
						return err
					}
					// Change the source volume's ownership if it differs from the root
					// files that where just copied
					if stat.Uid != srcStat.Uid || stat.Gid != srcStat.Gid {
						if err := os.Chown(srcPath, int(stat.Uid), int(stat.Gid)); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func (container *Container) applyExternalVolumes() error {
	if container.Config.VolumesFrom != "" {
		containerSpecs := strings.Split(container.Config.VolumesFrom, ",")
		for _, containerSpec := range containerSpecs {
			mountRW := true
			specParts := strings.SplitN(containerSpec, ":", 2)
			switch len(specParts) {
			case 0:
				return fmt.Errorf("Malformed volumes-from specification: %s", container.Config.VolumesFrom)
			case 2:
				switch specParts[1] {
				case "ro":
					mountRW = false
				case "rw": // mountRW is already true
				default:
					return fmt.Errorf("Malformed volumes-from speficication: %s", containerSpec)
				}
			}
			c := container.runtime.Get(specParts[0])
			if c == nil {
				return fmt.Errorf("Container %s not found. Impossible to mount its volumes", container.ID)
			}
			for volPath, id := range c.Volumes {
				if _, exists := container.Volumes[volPath]; exists {
					continue
				}
				if err := os.MkdirAll(path.Join(container.RootfsPath(), volPath), 0755); err != nil {
					return err
				}
				container.Volumes[volPath] = id
				if isRW, exists := c.VolumesRW[volPath]; exists {
					container.VolumesRW[volPath] = isRW && mountRW
				}
			}

		}
	}
	return nil
}

func (container *Container) Run() error {
	if err := container.Start(); err != nil {
		return err
	}
	container.Wait()
	return nil
}

func (container *Container) Output() (output []byte, err error) {
	pipe, err := container.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer pipe.Close()
	if err := container.Start(); err != nil {
		return nil, err
	}
	output, err = ioutil.ReadAll(pipe)
	container.Wait()
	return output, err
}

// Container.StdinPipe returns a WriteCloser which can be used to feed data
// to the standard input of the container's active process.
// Container.StdoutPipe and Container.StderrPipe each return a ReadCloser
// which can be used to retrieve the standard output (and error) generated
// by the container's active process. The output (and error) are actually
// copied and delivered to all StdoutPipe and StderrPipe consumers, using
// a kind of "broadcaster".

func (container *Container) StdinPipe() (io.WriteCloser, error) {
	return container.stdinPipe, nil
}

func (container *Container) StdoutPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	container.stdout.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (container *Container) StderrPipe() (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	container.stderr.AddWriter(writer, "")
	return utils.NewBufReader(reader), nil
}

func (container *Container) buildHostnameAndHostsFiles(IP string) {
	container.HostnamePath = path.Join(container.root, "hostname")
	ioutil.WriteFile(container.HostnamePath, []byte(container.Config.Hostname+"\n"), 0644)

	hostsContent := []byte(`
127.0.0.1	localhost
::1		localhost ip6-localhost ip6-loopback
fe00::0		ip6-localnet
ff00::0		ip6-mcastprefix
ff02::1		ip6-allnodes
ff02::2		ip6-allrouters
`)

	container.HostsPath = path.Join(container.root, "hosts")

	if container.Config.Domainname != "" {
		hostsContent = append([]byte(fmt.Sprintf("%s\t%s.%s %s\n", IP, container.Config.Hostname, container.Config.Domainname, container.Config.Hostname)), hostsContent...)
	} else {
		hostsContent = append([]byte(fmt.Sprintf("%s\t%s\n", IP, container.Config.Hostname)), hostsContent...)
	}

	ioutil.WriteFile(container.HostsPath, hostsContent, 0644)
}

func (container *Container) allocateNetwork() error {
	if container.Config.NetworkDisabled {
		return nil
	}

	var (
		iface *NetworkInterface
		err   error
	)
	// if container.State.IsGhost() {
	// 	if manager := container.runtime.networkManager; manager.disabled {
	// 		iface = &NetworkInterface{disabled: true}
	// 	} else {
	// 		iface = &NetworkInterface{
	// 			IPNet:   net.IPNet{IP: net.ParseIP(container.NetworkSettings.IPAddress), Mask: manager.bridgeNetwork.Mask},
	// 			Gateway: manager.bridgeNetwork.IP,
	// 			manager: manager,
	// 		}
	// 		if iface != nil && iface.IPNet.IP != nil {
	// 			ipNum := ipToInt(iface.IPNet.IP)
	// 			manager.ipAllocator.inUse[ipNum] = struct{}{}
	// 		} else {
	// 			iface, err = container.runtime.networkManager.Allocate()
	// 			if err != nil {
	// 				return err
	// 			}
	// 		}
	// 	}
	// } else {
	iface, err = container.runtime.networkManager.Allocate()
	if err != nil {
		return err
	}
	// }

	if container.Config.PortSpecs != nil {
		utils.Debugf("Migrating port mappings for container: %s", strings.Join(container.Config.PortSpecs, ", "))
		if err := migratePortMappings(container.Config, container.hostConfig); err != nil {
			return err
		}
		container.Config.PortSpecs = nil
		if err := container.writeHostConfig(); err != nil {
			return err
		}
	}

	var (
		portSpecs = make(map[Port]struct{})
		bindings  = make(map[Port][]PortBinding)
	)

	// if !container.State.IsGhost() {
	if container.Config.ExposedPorts != nil {
		portSpecs = container.Config.ExposedPorts
	}
	if container.hostConfig.PortBindings != nil {
		bindings = container.hostConfig.PortBindings
	}
	// } else {
	// 	if container.NetworkSettings.Ports != nil {
	// 		for port, binding := range container.NetworkSettings.Ports {
	// 			portSpecs[port] = struct{}{}
	// 			bindings[port] = binding
	// 		}
	// 	}
	// }

	container.NetworkSettings.PortMapping = nil

	for port := range portSpecs {
		binding := bindings[port]
		if container.hostConfig.PublishAllPorts && len(binding) == 0 {
			binding = append(binding, PortBinding{})
		}
		for i := 0; i < len(binding); i++ {
			b := binding[i]
			nat, err := iface.AllocatePort(port, b)
			if err != nil {
				iface.Release()
				return err
			}
			utils.Debugf("Allocate port: %s:%s->%s", nat.Binding.HostIp, port, nat.Binding.HostPort)
			binding[i] = nat.Binding
		}
		bindings[port] = binding
	}
	container.writeHostConfig()

	container.NetworkSettings.Ports = bindings
	container.network = iface

	container.NetworkSettings.Bridge = container.runtime.networkManager.bridgeIface
	container.NetworkSettings.IPAddress = iface.IPNet.IP.String()
	container.NetworkSettings.IPPrefixLen, _ = iface.IPNet.Mask.Size()
	container.NetworkSettings.Gateway = iface.Gateway.String()

	return nil
}

func (container *Container) releaseNetwork() {
	if container.Config.NetworkDisabled || container.network == nil {
		return
	}
	container.network.Release()
	container.network = nil
	container.NetworkSettings = &NetworkSettings{}
}

// FIXME: replace this with a control socket within dockerinit
func (container *Container) waitLxc() error {
	for {
		output, err := exec.Command("lxc-info", "-n", container.ID).CombinedOutput()
		if err != nil {
			return err
		}
		if !strings.Contains(string(output), "RUNNING") {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (container *Container) cleanup() {
	container.releaseNetwork()

	// Disable all active links
	if container.activeLinks != nil {
		for _, link := range container.activeLinks {
			link.Disable()
		}
	}

	if container.Config.OpenStdin {
		if err := container.stdin.Close(); err != nil {
			utils.Errorf("%s: Error close stdin: %s", container.ID, err)
		}
	}
	if err := container.stdout.CloseWriters(); err != nil {
		utils.Errorf("%s: Error close stdout: %s", container.ID, err)
	}
	if err := container.stderr.CloseWriters(); err != nil {
		utils.Errorf("%s: Error close stderr: %s", container.ID, err)
	}

	if container.ptyMaster != nil {
		if err := container.ptyMaster.Close(); err != nil {
			utils.Errorf("%s: Error closing Pty master: %s", container.ID, err)
		}
	}

	if err := container.Unmount(); err != nil {
		log.Printf("%v: Failed to umount filesystem: %v", container.ID, err)
	}
}

func (container *Container) kill(sig int) error {
	container.Lock()
	defer container.Unlock()

	// if !container.State.IsRunning() {
	// 	return nil
	// }

	if output, err := exec.Command("lxc-kill", "-n", container.ID, strconv.Itoa(sig)).CombinedOutput(); err != nil {
		log.Printf("error killing container %s (%s, %s)", utils.TruncateID(container.ID), output, err)
		return err
	}

	return nil
}

func (container *Container) Kill() error {
	// if !container.State.IsRunning() {
	// 	return nil
	// }

	// 1. Send SIGKILL
	if err := container.kill(9); err != nil {
		return err
	}

	// // 2. Wait for the process to die, in last resort, try to kill the process directly
	// if err := container.WaitTimeout(10 * time.Second); err != nil {
	// 	if container.cmd == nil {
	// 		return fmt.Errorf("lxc-kill failed, impossible to kill the container %s", utils.TruncateID(container.ID))
	// 	}
	// 	log.Printf("Container %s failed to exit within 10 seconds of lxc-kill %s - trying direct SIGKILL", "SIGKILL", utils.TruncateID(container.ID))
	// 	if err := container.cmd.Process.Kill(); err != nil {
	// 		return err
	// 	}
	// }

	container.Wait()
	return nil
}

func (container *Container) Stop(seconds int) error {
	// if !container.State.IsRunning() {
	// 	return nil
	// }

	// 1. Send a SIGTERM
	if err := container.kill(15); err != nil {
		utils.Debugf("Error sending kill SIGTERM: %s", err)
		log.Print("Failed to send SIGTERM to the process, force killing")
		if err := container.kill(9); err != nil {
			return err
		}
	}

	// 2. Wait for the process to exit on its own
	if err := container.WaitTimeout(time.Duration(seconds) * time.Second); err != nil {
		log.Printf("Container %v failed to exit within %d seconds of SIGTERM - using the force", container.ID, seconds)
		// 3. If it doesn't, then send SIGKILL
		if err := container.Kill(); err != nil {
			return err
		}
	}
	return nil
}

func (container *Container) Restart(seconds int) error {
	if err := container.Stop(seconds); err != nil {
		return err
	}
	return container.Start()
}

// Wait blocks until the container stops running, then returns its exit code.
func (container *Container) Wait() int {
	<-container.waitLock
	return 0
	// return container.State.GetExitCode()
}

func (container *Container) Resize(h, w int) error {
	pty, ok := container.ptyMaster.(*os.File)
	if !ok {
		return fmt.Errorf("ptyMaster does not have Fd() method")
	}
	return term.SetWinsize(pty.Fd(), &term.Winsize{Height: uint16(h), Width: uint16(w)})
}

func (container *Container) ExportRw() (archive.Archive, error) {
	if container.runtime == nil {
		return nil, fmt.Errorf("Can't load storage driver for unregistered container %s", container.ID)
	}

	return container.runtime.Diff(container)
}

func (container *Container) Export() (archive.Archive, error) {
	// if err := container.EnsureMounted(); err != nil {
	// 	return nil, err
	// }
	return archive.Tar(container.RootfsPath(), archive.Uncompressed)
}

func (container *Container) WaitTimeout(timeout time.Duration) error {
	done := make(chan bool)
	go func() {
		container.Wait()
		done <- true
	}()

	select {
	case <-time.After(timeout):
		return fmt.Errorf("Timed Out")
	case <-done:
		return nil
	}
}

func (container *Container) Changes() ([]archive.Change, error) {
	return container.runtime.Changes(container)
}

func (container *Container) GetImage() (*Image, error) {
	if container.runtime == nil {
		return nil, fmt.Errorf("Can't get image of unregistered container")
	}
	return container.runtime.graph.Get(container.Image)
}

func (container *Container) Unmount() error {
	return container.runtime.Unmount(container)
}

func (container *Container) logPath(name string) string {
	return path.Join(container.root, fmt.Sprintf("%s-%s.log", container.ID, name))
}

func (container *Container) ReadLog(name string) (io.Reader, error) {
	return os.Open(container.logPath(name))
}

func (container *Container) hostConfigPath() string {
	return path.Join(container.root, "hostconfig.json")
}

func (container *Container) jsonPath() string {
	return path.Join(container.root, "config.json")
}

func (container *Container) EnvConfigPath() string {
	return path.Join(container.root, "config.env")
}

func (container *Container) lxcConfigPath() string {
	return path.Join(container.root, "config.lxc")
}

// This method must be exported to be used from the lxc template
func (container *Container) RootfsPath() string {
	return container.rootfs
}

func validateID(id string) error {
	if id == "" {
		return fmt.Errorf("Invalid empty id")
	}
	return nil
}

// GetSize, return real size, virtual size
func (container *Container) GetSize() (int64, int64) {
	var (
		sizeRw, sizeRootfs int64
		err                error
		driver             = container.runtime.driver
	)

	// if err := container.EnsureMounted(); err != nil {
	// 	utils.Errorf("Warning: failed to compute size of container rootfs %s: %s", container.ID, err)
	// 	return sizeRw, sizeRootfs
	// }

	if differ, ok := container.runtime.driver.(graphdriver.Differ); ok {
		sizeRw, err = differ.DiffSize(container.ID)
		if err != nil {
			utils.Errorf("Warning: driver %s couldn't return diff size of container %s: %s", driver, container.ID, err)
			// FIXME: GetSize should return an error. Not changing it now in case
			// there is a side-effect.
			sizeRw = -1
		}
	} else {
		changes, _ := container.Changes()
		if changes != nil {
			sizeRw = archive.ChangesSize(container.RootfsPath(), changes)
		} else {
			sizeRw = -1
		}
	}

	if _, err = os.Stat(container.RootfsPath()); err != nil {
		if sizeRootfs, err = utils.TreeSize(container.RootfsPath()); err != nil {
			sizeRootfs = -1
		}
	}
	return sizeRw, sizeRootfs
}

func (container *Container) Copy(resource string) (archive.Archive, error) {
	// if err := container.EnsureMounted(); err != nil {
	// 	return nil, err
	// }
	var filter []string
	basePath := path.Join(container.RootfsPath(), resource)
	stat, err := os.Stat(basePath)
	if err != nil {
		return nil, err
	}
	if !stat.IsDir() {
		d, f := path.Split(basePath)
		basePath = d
		filter = []string{f}
	} else {
		filter = []string{path.Base(basePath)}
		basePath = path.Dir(basePath)
	}
	return archive.TarFilter(basePath, &archive.TarOptions{
		Compression: archive.Uncompressed,
		Includes:    filter,
		Recursive:   true,
	})
}

// Returns true if the container exposes a certain port
func (container *Container) Exposes(p Port) bool {
	_, exists := container.Config.ExposedPorts[p]
	return exists
}

func (container *Container) GetPtyMaster() (*os.File, error) {
	if container.ptyMaster == nil {
		return nil, ErrNoTTY
	}
	if pty, ok := container.ptyMaster.(*os.File); ok {
		return pty, nil
	}
	return nil, ErrNotATTY
}
