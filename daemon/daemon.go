package daemon

import (
	"container/list"
	"fmt"
	"github.com/dotcloud/docker/archive"
	"github.com/dotcloud/docker/daemon/execdriver"
	"github.com/dotcloud/docker/daemon/execdriver/execdrivers"
	"github.com/dotcloud/docker/daemon/execdriver/lxc"
	"github.com/dotcloud/docker/daemon/graphdriver"
	_ "github.com/dotcloud/docker/daemon/graphdriver/vfs"
	_ "github.com/dotcloud/docker/daemon/networkdriver/lxc"
	"github.com/dotcloud/docker/daemon/networkdriver/portallocator"
	"github.com/dotcloud/docker/daemonconfig"
	"github.com/dotcloud/docker/dockerversion"
	"github.com/dotcloud/docker/engine"
	"github.com/dotcloud/docker/graph"
	"github.com/dotcloud/docker/image"
	"github.com/dotcloud/docker/pkg/graphdb"
	"github.com/dotcloud/docker/pkg/sysinfo"
	"github.com/dotcloud/docker/runconfig"
	"github.com/dotcloud/docker/utils"
	"io"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Set the max depth to the aufs default that most
// kernels are compiled with
// For more information see: http://sourceforge.net/p/aufs/aufs3-standalone/ci/aufs3.12/tree/config.mk
const MaxImageDepth = 127

var (
	DefaultDns                = []string{"8.8.8.8", "8.8.4.4"}
	validContainerNameChars   = `[a-zA-Z0-9_.-]`
	validContainerNamePattern = regexp.MustCompile(`^/?` + validContainerNameChars + `+$`)
)

type Daemon struct {
	repository     string
	sysInitPath    string
	containers     *list.List
	graph          *graph.Graph
	repositories   *graph.TagStore
	idIndex        *utils.TruncIndex
	sysInfo        *sysinfo.SysInfo
	volumes        *graph.Graph
	srv            Server
	eng            *engine.Engine
	config         *daemonconfig.Config
	containerGraph *graphdb.Database
	driver         graphdriver.Driver
	execDriver     execdriver.Driver
}

// List returns an array of all containers registered in the daemon.
func (d *Daemon) List() []*Container {
	containers := new(History)
	for e := d.containers.Front(); e != nil; e = e.Next() {
		containers.Add(e.Value.(*Container))
	}
	return *containers
}

func (d *Daemon) getContainerElement(id string) *list.Element {
	for e := d.containers.Front(); e != nil; e = e.Next() {
		container := e.Value.(*Container)
		if container.ID == id {
			return e
		}
	}
	return nil
}

// Get looks for a container by the specified ID or name, and returns it.
// If the container is not found, or if an error occurs, nil is returned.
func (d *Daemon) Get(name string) *Container {
	if c, _ := d.GetByName(name); c != nil {
		return c
	}

	id, err := d.idIndex.Get(name)
	if err != nil {
		return nil
	}

	e := d.getContainerElement(id)
	if e == nil {
		return nil
	}
	return e.Value.(*Container)
}

// Exists returns a true if a container of the specified ID or name exists,
// false otherwise.
func (d *Daemon) Exists(id string) bool {
	return d.Get(id) != nil
}

func (d *Daemon) containerRoot(id string) string {
	return path.Join(d.repository, id)
}

// Load reads the contents of a container from disk
// This is typically done at startup.
func (d *Daemon) load(id string) (*Container, error) {
	container := &Container{root: d.containerRoot(id)}
	if err := container.FromDisk(); err != nil {
		return nil, err
	}
	if container.ID != id {
		return container, fmt.Errorf("Container %s is stored at %s", container.ID, id)
	}
	if container.State.IsRunning() {
		container.State.SetGhost(true)
	}
	return container, nil
}

// Register makes a container object usable by the daemon as <container.ID>
func (d *Daemon) Register(container *Container) error {
	if container.daemon != nil || d.Exists(container.ID) {
		return fmt.Errorf("Container is already loaded")
	}
	if err := validateID(container.ID); err != nil {
		return err
	}
	if err := d.ensureName(container); err != nil {
		return err
	}

	container.daemon = d

	// Attach to stdout and stderr
	container.stderr = utils.NewWriteBroadcaster()
	container.stdout = utils.NewWriteBroadcaster()
	// Attach to stdin
	if container.Config.OpenStdin {
		container.stdin, container.stdinPipe = io.Pipe()
	} else {
		container.stdinPipe = utils.NopWriteCloser(ioutil.Discard) // Silently drop stdin
	}
	// done
	d.containers.PushBack(container)
	d.idIndex.Add(container.ID)

	// FIXME: if the container is supposed to be running but is not, auto restart it?
	//        if so, then we need to restart monitor and init a new lock
	// If the container is supposed to be running, make sure of it
	if container.State.IsRunning() {
		if container.State.IsGhost() {
			utils.Debugf("killing ghost %s", container.ID)

			container.State.SetGhost(false)
			container.State.SetStopped(0)

			// We only have to handle this for lxc because the other drivers will ensure that
			// no ghost processes are left when docker dies
			if container.ExecDriver == "" || strings.Contains(container.ExecDriver, "lxc") {
				lxc.KillLxc(container.ID, 9)
				if err := container.Unmount(); err != nil {
					utils.Debugf("ghost unmount error %s", err)
				}
			}
		}

		info := d.execDriver.Info(container.ID)
		if !info.IsRunning() {
			utils.Debugf("Container %s was supposed to be running but is not.", container.ID)
			if d.config.AutoRestart {
				utils.Debugf("Restarting")
				if err := container.Unmount(); err != nil {
					utils.Debugf("restart unmount error %s", err)
				}

				container.State.SetGhost(false)
				container.State.SetStopped(0)
				if err := container.Start(); err != nil {
					return err
				}
			} else {
				utils.Debugf("Marking as stopped")
				container.State.SetStopped(-127)
				if err := container.ToDisk(); err != nil {
					return err
				}
			}
		}
	} else {
		// When the container is not running, we still initialize the waitLock
		// chan and close it. Receiving on nil chan blocks whereas receiving on a
		// closed chan does not. In this case we do not want to block.
		container.waitLock = make(chan struct{})
		close(container.waitLock)
	}
	return nil
}

func (d *Daemon) ensureName(container *Container) error {
	if container.Name == "" {
		name, err := generateRandomName(d)
		if err != nil {
			name = utils.TruncateID(container.ID)
		}
		container.Name = name

		if err := container.ToDisk(); err != nil {
			utils.Debugf("Error saving container name %s", err)
		}
		if !d.containerGraph.Exists(name) {
			if _, err := d.containerGraph.Set(name, container.ID); err != nil {
				utils.Debugf("Setting default id - %s", err)
			}
		}
	}
	return nil
}

func (d *Daemon) LogToDisk(src *utils.WriteBroadcaster, dst, stream string) error {
	log, err := os.OpenFile(dst, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	src.AddWriter(log, stream)
	return nil
}

// Destroy unregisters a container from the daemon and cleanly removes its contents from the filesystem.
func (d *Daemon) Destroy(container *Container) error {
	if container == nil {
		return fmt.Errorf("The given container is <nil>")
	}

	element := d.getContainerElement(container.ID)
	if element == nil {
		return fmt.Errorf("Container %v not found - maybe it was already destroyed?", container.ID)
	}

	if err := container.Stop(3); err != nil {
		return err
	}

	if err := d.driver.Remove(container.ID); err != nil {
		return fmt.Errorf("Driver %s failed to remove root filesystem %s: %s", d.driver, container.ID, err)
	}

	initID := fmt.Sprintf("%s-init", container.ID)
	if err := d.driver.Remove(initID); err != nil {
		return fmt.Errorf("Driver %s failed to remove init filesystem %s: %s", d.driver, initID, err)
	}

	if _, err := d.containerGraph.Purge(container.ID); err != nil {
		utils.Debugf("Unable to remove container from link graph: %s", err)
	}

	// Deregister the container before removing its directory, to avoid race conditions
	d.idIndex.Delete(container.ID)
	d.containers.Remove(element)
	if err := os.RemoveAll(container.root); err != nil {
		return fmt.Errorf("Unable to remove filesystem for %v: %v", container.ID, err)
	}
	return nil
}

func (d *Daemon) restore() error {
	if os.Getenv("DEBUG") == "" && os.Getenv("TEST") == "" {
		fmt.Printf("Loading containers: ")
	}
	dir, err := ioutil.ReadDir(d.repository)
	if err != nil {
		return err
	}
	containers := make(map[string]*Container)
	currentDriver := d.driver.String()

	for _, v := range dir {
		id := v.Name()
		container, err := d.load(id)
		if os.Getenv("DEBUG") == "" && os.Getenv("TEST") == "" {
			fmt.Print(".")
		}
		if err != nil {
			utils.Errorf("Failed to load container %v: %v", id, err)
			continue
		}

		// Ignore the container if it does not support the current driver being used by the graph
		if container.Driver == "" && currentDriver == "aufs" || container.Driver == currentDriver {
			utils.Debugf("Loaded container %v", container.ID)
			containers[container.ID] = container
		} else {
			utils.Debugf("Cannot load container %s because it was created with another graph driver.", container.ID)
		}
	}

	register := func(container *Container) {
		if err := d.Register(container); err != nil {
			utils.Debugf("Failed to register container %s: %s", container.ID, err)
		}
	}

	if entities := d.containerGraph.List("/", -1); entities != nil {
		for _, p := range entities.Paths() {
			if os.Getenv("DEBUG") == "" && os.Getenv("TEST") == "" {
				fmt.Print(".")
			}
			e := entities[p]
			if container, ok := containers[e.ID()]; ok {
				register(container)
				delete(containers, e.ID())
			}
		}
	}

	// Any containers that are left over do not exist in the graph
	for _, container := range containers {
		// Try to set the default name for a container if it exists prior to links
		container.Name, err = generateRandomName(d)
		if err != nil {
			container.Name = utils.TruncateID(container.ID)
		}

		if _, err := d.containerGraph.Set(container.Name, container.ID); err != nil {
			utils.Debugf("Setting default id - %s", err)
		}
		register(container)
	}

	if os.Getenv("DEBUG") == "" && os.Getenv("TEST") == "" {
		fmt.Printf(": done.\n")
	}

	return nil
}

// Create creates a new container from the given configuration with a given name.
func (d *Daemon) Create(config *runconfig.Config, name string) (*Container, []string, error) {
	// Lookup image
	img, err := d.repositories.LookupImage(config.Image)
	if err != nil {
		return nil, nil, err
	}

	// We add 2 layers to the depth because the container's rw and
	// init layer add to the restriction
	depth, err := img.Depth()
	if err != nil {
		return nil, nil, err
	}

	if depth+2 >= MaxImageDepth {
		return nil, nil, fmt.Errorf("Cannot create container with more than %d parents", MaxImageDepth)
	}

	checkDeprecatedExpose := func(config *runconfig.Config) bool {
		if config != nil {
			if config.PortSpecs != nil {
				for _, p := range config.PortSpecs {
					if strings.Contains(p, ":") {
						return true
					}
				}
			}
		}
		return false
	}

	warnings := []string{}
	if checkDeprecatedExpose(img.Config) || checkDeprecatedExpose(config) {
		warnings = append(warnings, "The mapping to public ports on your host via Dockerfile EXPOSE (host:port:port) has been deprecated. Use -p to publish the ports.")
	}

	if img.Config != nil {
		if err := runconfig.Merge(config, img.Config); err != nil {
			return nil, nil, err
		}
	}

	if len(config.Entrypoint) == 0 && len(config.Cmd) == 0 {
		return nil, nil, fmt.Errorf("No command specified")
	}

	// Generate id
	id := utils.GenerateRandomID()

	if name == "" {
		name, err = generateRandomName(d)
		if err != nil {
			name = utils.TruncateID(id)
		}
	} else {
		if !validContainerNamePattern.MatchString(name) {
			return nil, nil, fmt.Errorf("Invalid container name (%s), only %s are allowed", name, validContainerNameChars)
		}
	}

	if name[0] != '/' {
		name = "/" + name
	}

	// Set the enitity in the graph using the default name specified
	if _, err := d.containerGraph.Set(name, id); err != nil {
		if !graphdb.IsNonUniqueNameError(err) {
			return nil, nil, err
		}

		conflictingContainer, err := d.GetByName(name)
		if err != nil {
			if strings.Contains(err.Error(), "Could not find entity") {
				return nil, nil, err
			}

			// Remove name and continue starting the container
			if err := d.containerGraph.Delete(name); err != nil {
				return nil, nil, err
			}
		} else {
			nameAsKnownByUser := strings.TrimPrefix(name, "/")
			return nil, nil, fmt.Errorf(
				"Conflict, The name %s is already assigned to %s. You have to delete (or rename) that container to be able to assign %s to a container again.", nameAsKnownByUser,
				utils.TruncateID(conflictingContainer.ID), nameAsKnownByUser)
		}
	}

	// Generate default hostname
	// FIXME: the lxc template no longer needs to set a default hostname
	if config.Hostname == "" {
		config.Hostname = id[:12]
	}

	var args []string
	var entrypoint string

	if len(config.Entrypoint) != 0 {
		entrypoint = config.Entrypoint[0]
		args = append(config.Entrypoint[1:], config.Cmd...)
	} else {
		entrypoint = config.Cmd[0]
		args = config.Cmd[1:]
	}

	container := &Container{
		// FIXME: we should generate the ID here instead of receiving it as an argument
		ID:              id,
		Created:         time.Now().UTC(),
		Path:            entrypoint,
		Args:            args, //FIXME: de-duplicate from config
		Config:          config,
		hostConfig:      &runconfig.HostConfig{},
		Image:           img.ID, // Always use the resolved image id
		NetworkSettings: &NetworkSettings{},
		Name:            name,
		Driver:          d.driver.String(),
		ExecDriver:      d.execDriver.Name(),
	}
	container.root = d.containerRoot(container.ID)
	// Step 1: create the container directory.
	// This doubles as a barrier to avoid race conditions.
	if err := os.Mkdir(container.root, 0700); err != nil {
		return nil, nil, err
	}

	initID := fmt.Sprintf("%s-init", container.ID)
	if err := d.driver.Create(initID, img.ID); err != nil {
		return nil, nil, err
	}
	initPath, err := d.driver.Get(initID)
	if err != nil {
		return nil, nil, err
	}
	defer d.driver.Put(initID)

	if err := graph.SetupInitLayer(initPath); err != nil {
		return nil, nil, err
	}

	if err := d.driver.Create(container.ID, initID); err != nil {
		return nil, nil, err
	}
	resolvConf, err := utils.GetResolvConf()
	if err != nil {
		return nil, nil, err
	}

	if len(config.Dns) == 0 && len(d.config.Dns) == 0 && utils.CheckLocalDns(resolvConf) {
		d.config.Dns = DefaultDns
	}

	// If custom dns exists, then create a resolv.conf for the container
	if len(config.Dns) > 0 || len(d.config.Dns) > 0 {
		var dns []string
		if len(config.Dns) > 0 {
			dns = config.Dns
		} else {
			dns = d.config.Dns
		}
		container.ResolvConfPath = path.Join(container.root, "resolv.conf")
		f, err := os.Create(container.ResolvConfPath)
		if err != nil {
			return nil, nil, err
		}
		defer f.Close()
		for _, dns := range dns {
			if _, err := f.Write([]byte("nameserver " + dns + "\n")); err != nil {
				return nil, nil, err
			}
		}
	} else {
		container.ResolvConfPath = "/etc/resolv.conf"
	}

	// Step 2: save the container json
	if err := container.ToDisk(); err != nil {
		return nil, nil, err
	}

	// Step 3: register the container
	if err := d.Register(container); err != nil {
		return nil, nil, err
	}
	return container, warnings, nil
}

// Commit creates a new filesystem image from the current state of a container.
// The image can optionally be tagged into a repository
func (d *Daemon) Commit(container *Container, repository, tag, comment, author string, config *runconfig.Config) (*image.Image, error) {
	// FIXME: freeze the container before copying it to avoid data corruption?
	if err := container.Mount(); err != nil {
		return nil, err
	}
	defer container.Unmount()

	rwTar, err := container.ExportRw()
	if err != nil {
		return nil, err
	}
	defer rwTar.Close()

	// Create a new image from the container's base layers + a new layer from container changes
	var (
		containerID, containerImage string
		containerConfig             *runconfig.Config
	)
	if container != nil {
		containerID = container.ID
		containerImage = container.Image
		containerConfig = container.Config
	}
	img, err := d.graph.Create(rwTar, containerID, containerImage, comment, author, containerConfig, config)
	if err != nil {
		return nil, err
	}
	// Register the image if needed
	if repository != "" {
		if err := d.repositories.Set(repository, tag, img.ID, true); err != nil {
			return img, err
		}
	}
	return img, nil
}

func GetFullContainerName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("Container name cannot be empty")
	}
	if name[0] != '/' {
		name = "/" + name
	}
	return name, nil
}

func (d *Daemon) GetByName(name string) (*Container, error) {
	fullName, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	entity := d.containerGraph.Get(fullName)
	if entity == nil {
		return nil, fmt.Errorf("Could not find entity for %s", name)
	}
	e := d.getContainerElement(entity.ID())
	if e == nil {
		return nil, fmt.Errorf("Could not find container for entity id %s", entity.ID())
	}
	return e.Value.(*Container), nil
}

func (d *Daemon) Children(name string) (map[string]*Container, error) {
	name, err := GetFullContainerName(name)
	if err != nil {
		return nil, err
	}
	children := make(map[string]*Container)

	err = d.containerGraph.Walk(name, func(p string, e *graphdb.Entity) error {
		c := d.Get(e.ID())
		if c == nil {
			return fmt.Errorf("Could not get container for name %s and id %s", e.ID(), p)
		}
		children[p] = c
		return nil
	}, 0)

	if err != nil {
		return nil, err
	}
	return children, nil
}

func (d *Daemon) RegisterLink(parent, child *Container, alias string) error {
	fullName := path.Join(parent.Name, alias)
	if !d.containerGraph.Exists(fullName) {
		_, err := d.containerGraph.Set(fullName, child.ID)
		return err
	}
	return nil
}

// FIXME: harmonize with NewGraph()
func NewDaemon(config *daemonconfig.Config, eng *engine.Engine) (*Daemon, error) {
	daemon, err := NewDaemonFromDirectory(config, eng)
	if err != nil {
		return nil, err
	}
	return daemon, nil
}

func NewDaemonFromDirectory(config *daemonconfig.Config, eng *engine.Engine) (*Daemon, error) {

	// Set the default driver
	graphdriver.DefaultDriver = config.GraphDriver

	// Load storage driver
	driver, err := graphdriver.New(config.Root)
	if err != nil {
		return nil, err
	}
	utils.Debugf("Using graph driver %s", driver)

	daemonRepo := path.Join(config.Root, "containers")

	if err := os.MkdirAll(daemonRepo, 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}

	// Migrate the container if it is aufs and aufs is enabled
	if err = migrateIfAufs(driver, config.Root); err != nil {
		return nil, err
	}

	utils.Debugf("Creating images graph")
	g, err := graph.NewGraph(path.Join(config.Root, "graph"), driver)
	if err != nil {
		return nil, err
	}

	// We don't want to use a complex driver like aufs or devmapper
	// for volumes, just a plain filesystem
	volumesDriver, err := graphdriver.GetDriver("vfs", config.Root)
	if err != nil {
		return nil, err
	}
	utils.Debugf("Creating volumes graph")
	volumes, err := graph.NewGraph(path.Join(config.Root, "volumes"), volumesDriver)
	if err != nil {
		return nil, err
	}
	utils.Debugf("Creating repository list")
	repositories, err := graph.NewTagStore(path.Join(config.Root, "repositories-"+driver.String()), g)
	if err != nil {
		return nil, fmt.Errorf("Couldn't create Tag store: %s", err)
	}

	if !config.DisableNetwork {
		job := eng.Job("init_networkdriver")

		job.SetenvBool("EnableIptables", config.EnableIptables)
		job.SetenvBool("InterContainerCommunication", config.InterContainerCommunication)
		job.SetenvBool("EnableIpForward", config.EnableIpForward)
		job.Setenv("BridgeIface", config.BridgeIface)
		job.Setenv("BridgeIP", config.BridgeIP)
		job.Setenv("DefaultBindingIP", config.DefaultIp.String())

		if err := job.Run(); err != nil {
			return nil, err
		}
	}

	graphdbPath := path.Join(config.Root, "linkgraph.db")
	graph, err := graphdb.NewSqliteConn(graphdbPath)
	if err != nil {
		return nil, err
	}

	localCopy := path.Join(config.Root, "init", fmt.Sprintf("dockerinit-%s", dockerversion.VERSION))
	sysInitPath := utils.DockerInitPath(localCopy)
	if sysInitPath == "" {
		return nil, fmt.Errorf("Could not locate dockerinit: This usually means docker was built incorrectly. See http://docs.docker.io/en/latest/contributing/devenvironment for official build instructions.")
	}

	if sysInitPath != localCopy {
		// When we find a suitable dockerinit binary (even if it's our local binary), we copy it into config.Root at localCopy for future use (so that the original can go away without that being a problem, for example during a package upgrade).
		if err := os.Mkdir(path.Dir(localCopy), 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
		if _, err := utils.CopyFile(sysInitPath, localCopy); err != nil {
			return nil, err
		}
		if err := os.Chmod(localCopy, 0700); err != nil {
			return nil, err
		}
		sysInitPath = localCopy
	}

	sysInfo := sysinfo.New(false)
	ed, err := execdrivers.NewDriver(config.ExecDriver, config.Root, sysInitPath, sysInfo)
	if err != nil {
		return nil, err
	}

	d := &Daemon{
		repository:     daemonRepo,
		containers:     list.New(),
		graph:          g,
		repositories:   repositories,
		idIndex:        utils.NewTruncIndex(),
		sysInfo:        sysInfo,
		volumes:        volumes,
		config:         config,
		containerGraph: graph,
		driver:         driver,
		sysInitPath:    sysInitPath,
		execDriver:     ed,
		eng:            eng,
	}

	if err := d.restore(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *Daemon) Close() error {
	errorsStrings := []string{}
	if err := portallocator.ReleaseAll(); err != nil {
		utils.Errorf("portallocator.ReleaseAll(): %s", err)
		errorsStrings = append(errorsStrings, err.Error())
	}
	if err := d.driver.Cleanup(); err != nil {
		utils.Errorf("d.driver.Cleanup(): %s", err.Error())
		errorsStrings = append(errorsStrings, err.Error())
	}
	if err := d.containerGraph.Close(); err != nil {
		utils.Errorf("d.containerGraph.Close(): %s", err.Error())
		errorsStrings = append(errorsStrings, err.Error())
	}
	if len(errorsStrings) > 0 {
		return fmt.Errorf("%s", strings.Join(errorsStrings, ", "))
	}
	return nil
}

func (d *Daemon) Mount(container *Container) error {
	dir, err := d.driver.Get(container.ID)
	if err != nil {
		return fmt.Errorf("Error getting container %s from driver %s: %s", container.ID, d.driver, err)
	}
	if container.basefs == "" {
		container.basefs = dir
	} else if container.basefs != dir {
		return fmt.Errorf("Error: driver %s is returning inconsistent paths for container %s ('%s' then '%s')",
			d.driver, container.ID, container.basefs, dir)
	}
	return nil
}

func (d *Daemon) Unmount(container *Container) error {
	d.driver.Put(container.ID)
	return nil
}

func (d *Daemon) Changes(container *Container) ([]archive.Change, error) {
	if differ, ok := d.driver.(graphdriver.Differ); ok {
		return differ.Changes(container.ID)
	}
	cDir, err := d.driver.Get(container.ID)
	if err != nil {
		return nil, fmt.Errorf("Error getting container rootfs %s from driver %s: %s", container.ID, container.daemon.driver, err)
	}
	defer d.driver.Put(container.ID)
	initDir, err := d.driver.Get(container.ID + "-init")
	if err != nil {
		return nil, fmt.Errorf("Error getting container init rootfs %s from driver %s: %s", container.ID, container.daemon.driver, err)
	}
	defer d.driver.Put(container.ID + "-init")
	return archive.ChangesDirs(cDir, initDir)
}

func (d *Daemon) Diff(container *Container) (archive.Archive, error) {
	if differ, ok := d.driver.(graphdriver.Differ); ok {
		return differ.Diff(container.ID)
	}

	changes, err := d.Changes(container)
	if err != nil {
		return nil, err
	}

	cDir, err := d.driver.Get(container.ID)
	if err != nil {
		return nil, fmt.Errorf("Error getting container rootfs %s from driver %s: %s", container.ID, container.daemon.driver, err)
	}

	archive, err := archive.ExportChanges(cDir, changes)
	if err != nil {
		return nil, err
	}
	return utils.NewReadCloserWrapper(archive, func() error {
		err := archive.Close()
		d.driver.Put(container.ID)
		return err
	}), nil
}

func (d *Daemon) Run(c *Container, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (int, error) {
	return d.execDriver.Run(c.command, pipes, startCallback)
}

func (d *Daemon) Kill(c *Container, sig int) error {
	return d.execDriver.Kill(c.command, sig)
}

// Nuke kills all containers then removes all content
// from the content root, including images, volumes and
// container filesystems.
// Again: this will remove your entire docker daemon!
func (d *Daemon) Nuke() error {
	var wg sync.WaitGroup
	for _, container := range d.List() {
		wg.Add(1)
		go func(c *Container) {
			c.Kill()
			wg.Done()
		}(container)
	}
	wg.Wait()
	d.Close()

	return os.RemoveAll(d.config.Root)
}

// FIXME: this is a convenience function for integration tests
// which need direct access to d.graph.
// Once the tests switch to using engine and jobs, this method
// can go away.
func (d *Daemon) Graph() *graph.Graph {
	return d.graph
}

func (d *Daemon) Repositories() *graph.TagStore {
	return d.repositories
}

func (d *Daemon) Config() *daemonconfig.Config {
	return d.config
}

func (d *Daemon) SystemConfig() *sysinfo.SysInfo {
	return d.sysInfo
}

func (d *Daemon) SystemInitPath() string {
	return d.sysInitPath
}

func (d *Daemon) GraphDriver() graphdriver.Driver {
	return d.driver
}

func (d *Daemon) ExecutionDriver() execdriver.Driver {
	return d.execDriver
}

func (d *Daemon) Volumes() *graph.Graph {
	return d.volumes
}

func (d *Daemon) ContainerGraph() *graphdb.Database {
	return d.containerGraph
}

func (d *Daemon) SetServer(server Server) {
	d.srv = server
}

// History is a convenience type for storing a list of containers,
// ordered by creation date.
type History []*Container

func (history *History) Len() int {
	return len(*history)
}

func (history *History) Less(i, j int) bool {
	containers := *history
	return containers[j].When().Before(containers[i].When())
}

func (history *History) Swap(i, j int) {
	containers := *history
	tmp := containers[i]
	containers[i] = containers[j]
	containers[j] = tmp
}

func (history *History) Add(container *Container) {
	*history = append(*history, container)
	sort.Sort(history)
}
