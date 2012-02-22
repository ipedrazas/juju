package environs

import (
	"errors"
	"io"
	"launchpad.net/juju/go/schema"
	"launchpad.net/juju/go/state"
)

// A EnvironProvider represents a computing and storage provider.
type EnvironProvider interface {
	// ConfigChecker is used to check sections of the environments.yaml
	// file that specify this provider. The value passed to the Checker is
	// that returned from the yaml parse, of type schema.MapType.
	ConfigChecker() schema.Checker

	// NewEnviron creates a new Environ with
	// the given attributes returned by the ConfigChecker.
	// The name is that given in environments.yaml.
	Open(name string, attributes interface{}) (Environ, error)
}

// Instance represents the provider-specific notion of a machine.
type Instance interface {
	// Id returns a provider-generated identifier for the Instance.
	Id() string
	DNSName() (string, error)
}

var ErrMissingInstance = errors.New("one or more instance ids not found")

// An Environ represents a juju environment as specified
// in the environments.yaml file.
// 
// Due to the limitations of some providers (for example ec2), the
// results of the Environ methods may not be fully sequentially
// consistent. In particular, while a provider may retry when it
// gets an error for an operation, it will not retry when
// an operation succeeds, even if that success is not
// consistent with a previous operation.
// 
type Environ interface {
	// Bootstrap initializes the state for the environment,
	// possibly starting one or more instances.
	Bootstrap() error

	// StateInfo returns information on the state initialized
	// by Bootstrap.
	StateInfo() (*state.Info, error)

	// StartInstance asks for a new instance to be created,
	// associated with the provided machine identifier.
	// The given info describes the juju state for the new
	// instance to connect to.
	// TODO add arguments to specify type of new machine.
	StartInstance(machineId int, info *state.Info) (Instance, error)

	// StopInstances shuts down the given instances.
	StopInstances([]Instance) error

	// Instances returns a slice of instances corresponding to
	// the given instance ids. If some (but not all) of the instances are not
	// found, the returned slice will have nil Inststances in those
	// slots, and ErrMissingInstance will be returned.
	Instances(ids []string) ([]Instance, error)

	// Put reads from r and writes to the given file in the
	// environment's storage. The length must give the total
	// length of the file.
	PutFile(file string, r io.Reader, length int64) error

	// Get opens the given file in the environment's storage
	// and returns a ReadCloser that can be used to read its
	// contents. It is the caller's responsibility to close it
	// after use.
	GetFile(file string) (io.ReadCloser, error)

	// RemoveFile removes the given file from the environment's storage.
	// It is not an error to remove a file that does not exist.
	RemoveFile(file string) error

	// Destroy shuts down all known machines and destroys the
	// rest of the environment. A list of instances known to
	// be part of the environment can be given with insts.
	// This is because recently started machines might not
	// yet be visible in the environment, so this method
	// can wait until they are.
	Destroy(insts []Instance) error
}
