package fsrepo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	dir "github.com/ipfs/go-ipfs/thirdparty/dir"
	"github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-datastore/measure"
	repo "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/repo"
	"github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/repo/common"
	config "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/repo/config"
	lockfile "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/repo/fsrepo/lock"
	mfsr "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/repo/fsrepo/migrations"
	serialize "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/repo/fsrepo/serialize"
	util "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/util"
	logging "github.com/noffle/ipget/Godeps/_workspace/src/github.com/ipfs/go-ipfs/vendor/QmQg1J6vikuXF9oDvm4wpdeAUvvkVEKW1EYDw9HhTMnP2b/go-log"
)

var log = logging.Logger("fsrepo")

// version number that we are currently expecting to see
var RepoVersion = "3"

var migrationInstructions = `See https://github.com/ipfs/fs-repo-migrations/blob/master/run.md
Sorry for the inconvenience. In the future, these will run automatically.`

var errIncorrectRepoFmt = `Repo has incorrect version: %s
Program version is: %s
Please run the ipfs migration tool before continuing.
` + migrationInstructions

var (
	ErrNoVersion = errors.New("no version file found, please run 0-to-1 migration tool.\n" + migrationInstructions)
	ErrOldRepo   = errors.New("ipfs repo found in old '~/.go-ipfs' location, please run migration tool.\n" + migrationInstructions)
)

type NoRepoError struct {
	Path string
}

var _ error = NoRepoError{}

func (err NoRepoError) Error() string {
	return fmt.Sprintf("no ipfs repo found in %s.\nplease run: ipfs init", err.Path)
}

const apiFile = "api"

var (

	// packageLock must be held to while performing any operation that modifies an
	// FSRepo's state field. This includes Init, Open, Close, and Remove.
	packageLock sync.Mutex

	// onlyOne keeps track of open FSRepo instances.
	//
	// TODO: once command Context / Repo integration is cleaned up,
	// this can be removed. Right now, this makes ConfigCmd.Run
	// function try to open the repo twice:
	//
	//     $ ipfs daemon &
	//     $ ipfs config foo
	//
	// The reason for the above is that in standalone mode without the
	// daemon, `ipfs config` tries to save work by not building the
	// full IpfsNode, but accessing the Repo directly.
	onlyOne repo.OnlyOne
)

// FSRepo represents an IPFS FileSystem Repo. It is safe for use by multiple
// callers.
type FSRepo struct {
	// has Close been called already
	closed bool
	// path is the file-system path
	path string
	// lockfile is the file system lock to prevent others from opening
	// the same fsrepo path concurrently
	lockfile io.Closer
	config   *config.Config
	ds       repo.Datastore
}

var _ repo.Repo = (*FSRepo)(nil)

// Open the FSRepo at path. Returns an error if the repo is not
// initialized.
func Open(repoPath string) (repo.Repo, error) {
	fn := func() (repo.Repo, error) {
		return open(repoPath)
	}
	return onlyOne.Open(repoPath, fn)
}

func open(repoPath string) (repo.Repo, error) {
	packageLock.Lock()
	defer packageLock.Unlock()

	r, err := newFSRepo(repoPath)
	if err != nil {
		return nil, err
	}

	// Check if its initialized
	if err := checkInitialized(r.path); err != nil {
		return nil, err
	}

	r.lockfile, err = lockfile.Lock(r.path)
	if err != nil {
		return nil, err
	}
	keepLocked := false
	defer func() {
		// unlock on error, leave it locked on success
		if !keepLocked {
			r.lockfile.Close()
		}
	}()

	// Check version, and error out if not matching
	ver, err := mfsr.RepoPath(r.path).Version()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoVersion
		}
		return nil, err
	}

	if ver != RepoVersion {
		return nil, fmt.Errorf(errIncorrectRepoFmt, ver, RepoVersion)
	}

	// check repo path, then check all constituent parts.
	if err := dir.Writable(r.path); err != nil {
		return nil, err
	}

	if err := r.openConfig(); err != nil {
		return nil, err
	}

	if err := r.openDatastore(); err != nil {
		return nil, err
	}

	keepLocked = true
	return r, nil
}

func newFSRepo(rpath string) (*FSRepo, error) {
	expPath, err := util.TildeExpansion(filepath.Clean(rpath))
	if err != nil {
		return nil, err
	}

	return &FSRepo{path: expPath}, nil
}

func checkInitialized(path string) error {
	if !isInitializedUnsynced(path) {
		alt := strings.Replace(path, ".ipfs", ".go-ipfs", 1)
		if isInitializedUnsynced(alt) {
			return ErrOldRepo
		}
		return NoRepoError{Path: path}
	}
	return nil
}

// ConfigAt returns an error if the FSRepo at the given path is not
// initialized. This function allows callers to read the config file even when
// another process is running and holding the lock.
func ConfigAt(repoPath string) (*config.Config, error) {

	// packageLock must be held to ensure that the Read is atomic.
	packageLock.Lock()
	defer packageLock.Unlock()

	configFilename, err := config.Filename(repoPath)
	if err != nil {
		return nil, err
	}
	return serialize.Load(configFilename)
}

// configIsInitialized returns true if the repo is initialized at
// provided |path|.
func configIsInitialized(path string) bool {
	configFilename, err := config.Filename(path)
	if err != nil {
		return false
	}
	if !util.FileExists(configFilename) {
		return false
	}
	return true
}

func initConfig(path string, conf *config.Config) error {
	if configIsInitialized(path) {
		return nil
	}
	configFilename, err := config.Filename(path)
	if err != nil {
		return err
	}
	// initialization is the one time when it's okay to write to the config
	// without reading the config from disk and merging any user-provided keys
	// that may exist.
	if err := serialize.WriteConfigFile(configFilename, conf); err != nil {
		return err
	}
	return nil
}

// Init initializes a new FSRepo at the given path with the provided config.
// TODO add support for custom datastores.
func Init(repoPath string, conf *config.Config) error {

	// packageLock must be held to ensure that the repo is not initialized more
	// than once.
	packageLock.Lock()
	defer packageLock.Unlock()

	if isInitializedUnsynced(repoPath) {
		return nil
	}

	if err := initConfig(repoPath, conf); err != nil {
		return err
	}

	if err := initDefaultDatastore(repoPath, conf); err != nil {
		return err
	}

	if err := dir.Writable(filepath.Join(repoPath, "logs")); err != nil {
		return err
	}

	if err := mfsr.RepoPath(repoPath).WriteVersion(RepoVersion); err != nil {
		return err
	}

	return nil
}

// Remove recursively removes the FSRepo at |path|.
func Remove(repoPath string) error {
	repoPath = filepath.Clean(repoPath)
	return os.RemoveAll(repoPath)
}

// LockedByOtherProcess returns true if the FSRepo is locked by another
// process. If true, then the repo cannot be opened by this process.
func LockedByOtherProcess(repoPath string) (bool, error) {
	repoPath = filepath.Clean(repoPath)
	// NB: the lock is only held when repos are Open
	return lockfile.Locked(repoPath)
}

// APIAddr returns the registered API addr, according to the api file
// in the fsrepo. This is a concurrent operation, meaning that any
// process may read this file. modifying this file, therefore, should
// use "mv" to replace the whole file and avoid interleaved read/writes.
func APIAddr(repoPath string) (string, error) {
	repoPath = filepath.Clean(repoPath)
	apiFilePath := filepath.Join(repoPath, apiFile)

	// if there is no file, assume there is no api addr.
	f, err := os.Open(apiFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", repo.ErrApiNotRunning
		}
		return "", err
	}
	defer f.Close()

	// read up to 2048 bytes. io.ReadAll is a vulnerability, as
	// someone could hose the process by putting a massive file there.
	buf := make([]byte, 2048)
	n, err := f.Read(buf)
	if err != nil && err != io.EOF {
		return "", err
	}

	s := string(buf[:n])
	s = strings.TrimSpace(s)
	return s, nil
}

// SetAPIAddr writes the API Addr to the /api file.
func (r *FSRepo) SetAPIAddr(addr string) error {
	f, err := os.Create(filepath.Join(r.path, apiFile))
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(addr)
	return err
}

// openConfig returns an error if the config file is not present.
func (r *FSRepo) openConfig() error {
	configFilename, err := config.Filename(r.path)
	if err != nil {
		return err
	}
	conf, err := serialize.Load(configFilename)
	if err != nil {
		return err
	}
	r.config = conf
	return nil
}

// openDatastore returns an error if the config file is not present.
func (r *FSRepo) openDatastore() error {
	switch r.config.Datastore.Type {
	case "default", "leveldb", "":
		d, err := openDefaultDatastore(r)
		if err != nil {
			return err
		}
		r.ds = d
	case "s3":
		var dscfg config.S3Datastore
		if err := json.Unmarshal(r.config.Datastore.ParamData(), &dscfg); err != nil {
			return fmt.Errorf("datastore s3: %v", err)
		}

		ds, err := openS3Datastore(dscfg)
		if err != nil {
			return err
		}

		r.ds = ds
	default:
		return fmt.Errorf("unknown datastore type: %s", r.config.Datastore.Type)
	}

	// Wrap it with metrics gathering
	//
	// Add our PeerID to metrics paths to keep them unique
	//
	// As some tests just pass a zero-value Config to fsrepo.Init,
	// cope with missing PeerID.
	id := r.config.Identity.PeerID
	if id == "" {
		// the tests pass in a zero Config; cope with it
		id = fmt.Sprintf("uninitialized_%p", r)
	}
	prefix := "fsrepo." + id + ".datastore"
	r.ds = measure.New(prefix, r.ds)

	return nil
}

// Close closes the FSRepo, releasing held resources.
func (r *FSRepo) Close() error {
	packageLock.Lock()
	defer packageLock.Unlock()

	if r.closed {
		return errors.New("repo is closed")
	}

	err := os.Remove(filepath.Join(r.path, apiFile))
	if err != nil {
		log.Warning("error removing api file: ", err)
	}

	if err := r.ds.Close(); err != nil {
		return err
	}

	// This code existed in the previous versions, but
	// EventlogComponent.Close was never called. Preserving here
	// pending further discussion.
	//
	// TODO It isn't part of the current contract, but callers may like for us
	// to disable logging once the component is closed.
	// logging.Configure(logging.Output(os.Stderr))

	r.closed = true
	if err := r.lockfile.Close(); err != nil {
		return err
	}
	return nil
}

// Result when not Open is undefined. The method may panic if it pleases.
func (r *FSRepo) Config() (*config.Config, error) {

	// It is not necessary to hold the package lock since the repo is in an
	// opened state. The package lock is _not_ meant to ensure that the repo is
	// thread-safe. The package lock is only meant to guard againt removal and
	// coordinate the lockfile. However, we provide thread-safety to keep
	// things simple.
	packageLock.Lock()
	defer packageLock.Unlock()

	if r.closed {
		return nil, errors.New("cannot access config, repo not open")
	}
	return r.config, nil
}

// setConfigUnsynced is for private use.
func (r *FSRepo) setConfigUnsynced(updated *config.Config) error {
	configFilename, err := config.Filename(r.path)
	if err != nil {
		return err
	}
	// to avoid clobbering user-provided keys, must read the config from disk
	// as a map, write the updated struct values to the map and write the map
	// to disk.
	var mapconf map[string]interface{}
	if err := serialize.ReadConfigFile(configFilename, &mapconf); err != nil {
		return err
	}
	m, err := config.ToMap(updated)
	if err != nil {
		return err
	}
	for k, v := range m {
		mapconf[k] = v
	}
	if err := serialize.WriteConfigFile(configFilename, mapconf); err != nil {
		return err
	}
	*r.config = *updated // copy so caller cannot modify this private config
	return nil
}

// SetConfig updates the FSRepo's config.
func (r *FSRepo) SetConfig(updated *config.Config) error {

	// packageLock is held to provide thread-safety.
	packageLock.Lock()
	defer packageLock.Unlock()

	return r.setConfigUnsynced(updated)
}

// GetConfigKey retrieves only the value of a particular key.
func (r *FSRepo) GetConfigKey(key string) (interface{}, error) {
	packageLock.Lock()
	defer packageLock.Unlock()

	if r.closed {
		return nil, errors.New("repo is closed")
	}

	filename, err := config.Filename(r.path)
	if err != nil {
		return nil, err
	}
	var cfg map[string]interface{}
	if err := serialize.ReadConfigFile(filename, &cfg); err != nil {
		return nil, err
	}
	return common.MapGetKV(cfg, key)
}

// SetConfigKey writes the value of a particular key.
func (r *FSRepo) SetConfigKey(key string, value interface{}) error {
	packageLock.Lock()
	defer packageLock.Unlock()

	if r.closed {
		return errors.New("repo is closed")
	}

	filename, err := config.Filename(r.path)
	if err != nil {
		return err
	}
	var mapconf map[string]interface{}
	if err := serialize.ReadConfigFile(filename, &mapconf); err != nil {
		return err
	}

	// Get the type of the value associated with the key
	oldValue, err := common.MapGetKV(mapconf, key)
	ok := true
	if err != nil {
		// key-value does not exist yet
		switch v := value.(type) {
		case string:
			value, err = strconv.ParseBool(v)
			if err != nil {
				value, err = strconv.Atoi(v)
				if err != nil {
					value, err = strconv.ParseFloat(v, 32)
					if err != nil {
						value = v
					}
				}
			}
		default:
		}
	} else {
		switch oldValue.(type) {
		case bool:
			value, ok = value.(bool)
		case int:
			value, ok = value.(int)
		case float32:
			value, ok = value.(float32)
		case string:
			value, ok = value.(string)
		default:
			value = value
		}
		if !ok {
			return fmt.Errorf("Wrong config type, expected %T", oldValue)
		}
	}

	if err := common.MapSetKV(mapconf, key, value); err != nil {
		return err
	}

	// This step doubles as to validate the map against the struct
	// before serialization
	conf, err := config.FromMap(mapconf)
	if err != nil {
		return err
	}
	if err := serialize.WriteConfigFile(filename, mapconf); err != nil {
		return err
	}
	return r.setConfigUnsynced(conf) // TODO roll this into this method
}

// Datastore returns a repo-owned datastore. If FSRepo is Closed, return value
// is undefined.
func (r *FSRepo) Datastore() repo.Datastore {
	packageLock.Lock()
	d := r.ds
	packageLock.Unlock()
	return d
}

// GetStorageUsage computes the storage space taken by the repo in bytes
func (r *FSRepo) GetStorageUsage() (uint64, error) {
	pth, err := config.PathRoot()
	if err != nil {
		return 0, err
	}

	var du uint64
	err = filepath.Walk(pth, func(p string, f os.FileInfo, err error) error {
		du += uint64(f.Size())
		return nil
	})
	return du, err
}

var _ io.Closer = &FSRepo{}
var _ repo.Repo = &FSRepo{}

// IsInitialized returns true if the repo is initialized at provided |path|.
func IsInitialized(path string) bool {
	// packageLock is held to ensure that another caller doesn't attempt to
	// Init or Remove the repo while this call is in progress.
	packageLock.Lock()
	defer packageLock.Unlock()

	return isInitializedUnsynced(path)
}

// private methods below this point. NB: packageLock must held by caller.

// isInitializedUnsynced reports whether the repo is initialized. Caller must
// hold the packageLock.
func isInitializedUnsynced(repoPath string) bool {
	if !configIsInitialized(repoPath) {
		return false
	}

	if !util.FileExists(filepath.Join(repoPath, leveldbDirectory)) {
		return false
	}

	return true
}