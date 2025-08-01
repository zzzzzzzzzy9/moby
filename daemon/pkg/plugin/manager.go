package plugin

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/log"
	"github.com/moby/moby/api/types"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/v2/daemon/internal/containerfs"
	"github.com/moby/moby/v2/daemon/internal/lazyregexp"
	v2 "github.com/moby/moby/v2/daemon/pkg/plugin/v2"
	"github.com/moby/moby/v2/daemon/pkg/registry"
	"github.com/moby/moby/v2/pkg/authorization"
	"github.com/moby/pubsub"
	"github.com/moby/sys/atomicwriter"
	"github.com/opencontainers/go-digest"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	configFileName = "config.json"
	rootFSFileName = "rootfs"
)

var validFullID = lazyregexp.New(`^([a-f0-9]{64})$`)

// Executor is the interface that the plugin manager uses to interact with for starting/stopping plugins
type Executor interface {
	Create(id string, spec specs.Spec, stdout, stderr io.WriteCloser) error
	IsRunning(id string) (bool, error)
	Restore(id string, stdout, stderr io.WriteCloser) (alive bool, err error)
	Signal(id string, signal syscall.Signal) error
}

// EndpointResolver provides looking up registry endpoints for pulling.
type EndpointResolver interface {
	LookupPullEndpoints(hostname string) (endpoints []registry.APIEndpoint, err error)
}

func (pm *Manager) restorePlugin(p *v2.Plugin, c *controller) error {
	if p.IsEnabled() {
		return pm.restore(p, c)
	}
	return nil
}

type eventLogger func(id, name string, action events.Action)

// ManagerConfig defines configuration needed to start new manager.
type ManagerConfig struct {
	Store              *Store // remove
	RegistryService    EndpointResolver
	LiveRestoreEnabled bool // TODO: remove
	LogPluginEvent     eventLogger
	Root               string
	ExecRoot           string
	CreateExecutor     ExecutorCreator
	AuthzMiddleware    *authorization.Middleware
}

// ExecutorCreator is used in the manager config to pass in an `Executor`
type ExecutorCreator func(*Manager) (Executor, error)

// Manager controls the plugin subsystem.
type Manager struct {
	config    ManagerConfig
	mu        sync.RWMutex // protects cMap
	muGC      sync.RWMutex // protects blobstore deletions
	cMap      map[*v2.Plugin]*controller
	blobStore content.Store
	publisher *pubsub.Publisher
	executor  Executor
}

// controller represents the manager's control on a plugin.
type controller struct {
	restart       bool
	exitChan      chan bool
	timeoutInSecs int
}

// NewManager returns a new plugin manager.
func NewManager(config ManagerConfig) (*Manager, error) {
	manager := &Manager{
		config: config,
	}
	for _, dirName := range []string{manager.config.Root, manager.config.ExecRoot, manager.tmpDir()} {
		if err := os.MkdirAll(dirName, 0o700); err != nil {
			return nil, errors.Wrapf(err, "failed to mkdir %v", dirName)
		}
	}
	var err error
	manager.executor, err = config.CreateExecutor(manager)
	if err != nil {
		return nil, err
	}

	manager.blobStore, err = local.NewStore(filepath.Join(manager.config.Root, "storage"))
	if err != nil {
		return nil, errors.Wrap(err, "error creating plugin blob store")
	}

	manager.cMap = make(map[*v2.Plugin]*controller)
	if err := manager.reload(); err != nil {
		return nil, errors.Wrap(err, "failed to restore plugins")
	}

	manager.publisher = pubsub.NewPublisher(0, 0)
	return manager, nil
}

func (pm *Manager) tmpDir() string {
	return filepath.Join(pm.config.Root, "tmp")
}

// HandleExitEvent is called when the executor receives the exit event
// In the future we may change this, but for now all we care about is the exit event.
func (pm *Manager) HandleExitEvent(id string) error {
	p, err := pm.config.Store.GetV2Plugin(id)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(filepath.Join(pm.config.ExecRoot, id)); err != nil {
		log.G(context.TODO()).WithError(err).WithField("id", id).Error("Could not remove plugin bundle dir")
	}

	pm.mu.RLock()
	c := pm.cMap[p]
	if c.exitChan != nil {
		close(c.exitChan)
		c.exitChan = nil // ignore duplicate events (containerd issue #2299)
	}
	restart := c.restart
	pm.mu.RUnlock()

	if restart {
		pm.enable(p, c, true)
	} else if err := recursiveUnmount(filepath.Join(pm.config.Root, id)); err != nil {
		return errors.Wrap(err, "error cleaning up plugin mounts")
	}
	return nil
}

func handleLoadError(err error, id string) {
	if err == nil {
		return
	}
	logger := log.G(context.TODO()).WithError(err).WithField("id", id)
	if errors.Is(err, os.ErrNotExist) {
		// Likely some error while removing on an older version of docker
		logger.Warn("missing plugin config, skipping: this may be caused due to a failed remove and requires manual cleanup.")
		return
	}
	logger.Error("error loading plugin, skipping")
}

func (pm *Manager) reload() error { // todo: restore
	dir, err := os.ReadDir(pm.config.Root)
	if err != nil {
		return errors.Wrapf(err, "failed to read %v", pm.config.Root)
	}
	plugins := make(map[string]*v2.Plugin)
	for _, v := range dir {
		if validFullID.MatchString(v.Name()) {
			p, err := pm.loadPlugin(v.Name())
			if err != nil {
				handleLoadError(err, v.Name())
				continue
			}
			plugins[p.GetID()] = p
		} else {
			if validFullID.MatchString(strings.TrimSuffix(v.Name(), "-removing")) {
				// There was likely some error while removing this plugin, let's try to remove again here
				if err := containerfs.EnsureRemoveAll(v.Name()); err != nil {
					log.G(context.TODO()).WithError(err).WithField("id", v.Name()).Warn("error while attempting to clean up previously removed plugin")
				}
			}
		}
	}

	pm.config.Store.SetAll(plugins)

	var wg sync.WaitGroup
	wg.Add(len(plugins))
	for _, p := range plugins {
		c := &controller{exitChan: make(chan bool)}
		pm.mu.Lock()
		pm.cMap[p] = c
		pm.mu.Unlock()

		go func(p *v2.Plugin) {
			defer wg.Done()
			// TODO(thaJeztah): make this fail if the plugin has "graphdriver" capability ?
			if err := pm.restorePlugin(p, c); err != nil {
				log.G(context.TODO()).WithError(err).WithField("id", p.GetID()).Error("Failed to restore plugin")
				return
			}

			if p.Rootfs != "" {
				p.Rootfs = filepath.Join(pm.config.Root, p.PluginObj.ID, "rootfs")
			}

			// We should only enable rootfs propagation for certain plugin types that need it.
			for _, typ := range p.PluginObj.Config.Interface.Types {
				if (typ.Capability == "volumedriver" || typ.Capability == "graphdriver" || typ.Capability == "csinode" || typ.Capability == "csicontroller") && typ.Prefix == "docker" && strings.HasPrefix(typ.Version, "1.") {
					if p.PluginObj.Config.PropagatedMount != "" {
						propRoot := filepath.Join(filepath.Dir(p.Rootfs), "propagated-mount")

						if typ.Capability == "graphdriver" {
							// TODO(thaJeztah): remove this for next release.
							log.G(context.TODO()).WithError(err).WithField("dir", propRoot).Warn("skipping migrating propagated mount storage for deprecated graphdriver plugin")
						}

						// check if we need to migrate an older propagated mount from before
						// these mounts were stored outside the plugin rootfs
						if _, err := os.Stat(propRoot); os.IsNotExist(err) {
							rootfsProp := filepath.Join(p.Rootfs, p.PluginObj.Config.PropagatedMount)
							if _, err := os.Stat(rootfsProp); err == nil {
								if err := os.Rename(rootfsProp, propRoot); err != nil {
									log.G(context.TODO()).WithError(err).WithField("dir", propRoot).Error("error migrating propagated mount storage")
								}
							}
						}

						if err := os.MkdirAll(propRoot, 0o755); err != nil {
							log.G(context.TODO()).Errorf("failed to create PropagatedMount directory at %s: %v", propRoot, err)
						}
					}
				}
			}

			pm.save(p)
			requiresManualRestore := !pm.config.LiveRestoreEnabled && p.IsEnabled()

			if requiresManualRestore {
				// if liveRestore is not enabled, the plugin will be stopped now so we should enable it
				if err := pm.enable(p, c, true); err != nil {
					log.G(context.TODO()).WithError(err).WithField("id", p.GetID()).Error("failed to enable plugin")
				}
			}
		}(p)
	}
	wg.Wait()
	return nil
}

// Get looks up the requested plugin in the store.
func (pm *Manager) Get(idOrName string) (*v2.Plugin, error) {
	return pm.config.Store.GetV2Plugin(idOrName)
}

func (pm *Manager) loadPlugin(id string) (*v2.Plugin, error) {
	p := filepath.Join(pm.config.Root, id, configFileName)
	dt, err := os.ReadFile(p)
	if err != nil {
		return nil, errors.Wrapf(err, "error reading %v", p)
	}
	var plugin v2.Plugin
	if err := json.Unmarshal(dt, &plugin); err != nil {
		return nil, errors.Wrapf(err, "error decoding %v", p)
	}
	return &plugin, nil
}

func (pm *Manager) save(p *v2.Plugin) error {
	pluginJSON, err := json.Marshal(p)
	if err != nil {
		return errors.Wrap(err, "failed to marshal plugin json")
	}
	if err := atomicwriter.WriteFile(filepath.Join(pm.config.Root, p.GetID(), configFileName), pluginJSON, 0o600); err != nil {
		return errors.Wrap(err, "failed to write atomically plugin json")
	}
	return nil
}

// GC cleans up unreferenced blobs. This is recommended to run in a goroutine
func (pm *Manager) GC() {
	pm.muGC.Lock()
	defer pm.muGC.Unlock()

	used := make(map[digest.Digest]struct{})
	for _, p := range pm.config.Store.GetAll() {
		used[p.Config] = struct{}{}
		for _, b := range p.Blobsums {
			used[b] = struct{}{}
		}
	}

	ctx := context.TODO()
	pm.blobStore.Walk(ctx, func(info content.Info) error {
		_, ok := used[info.Digest]
		if ok {
			return nil
		}

		return pm.blobStore.Delete(ctx, info.Digest)
	})
}

type logHook struct{ id string }

func (logHook) Levels() []log.Level {
	return []log.Level{
		log.PanicLevel,
		log.FatalLevel,
		log.ErrorLevel,
		log.WarnLevel,
		log.InfoLevel,
		log.DebugLevel,
		log.TraceLevel,
	}
}

func (l logHook) Fire(entry *log.Entry) error {
	entry.Data = log.Fields{"plugin": l.id}
	return nil
}

func makeLoggerStreams(id string) (stdout, stderr io.WriteCloser) {
	logger := logrus.New()
	logger.Hooks.Add(logHook{id})
	return logger.WriterLevel(log.InfoLevel), logger.WriterLevel(log.ErrorLevel)
}

func validatePrivileges(requiredPrivileges, privileges types.PluginPrivileges) error {
	if !isEqual(requiredPrivileges, privileges, isEqualPrivilege) {
		return errors.New("incorrect privileges")
	}

	return nil
}

func isEqual(arrOne, arrOther types.PluginPrivileges, compare func(x, y types.PluginPrivilege) bool) bool {
	if len(arrOne) != len(arrOther) {
		return false
	}

	sort.Sort(arrOne)
	sort.Sort(arrOther)

	for i := 1; i < arrOne.Len(); i++ {
		if !compare(arrOne[i], arrOther[i]) {
			return false
		}
	}

	return true
}

func isEqualPrivilege(a, b types.PluginPrivilege) bool {
	if a.Name != b.Name {
		return false
	}

	return reflect.DeepEqual(a.Value, b.Value)
}
