// Code generated by lister-gen. DO NOT EDIT.

package v1alpha1

import (
	v1alpha1 "github.com/openshift/api/machineconfiguration/v1alpha1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

// MachineOSConfigLister helps list MachineOSConfigs.
// All objects returned here must be treated as read-only.
type MachineOSConfigLister interface {
	// List lists all MachineOSConfigs in the indexer.
	// Objects returned here must be treated as read-only.
	List(selector labels.Selector) (ret []*v1alpha1.MachineOSConfig, err error)
	// Get retrieves the MachineOSConfig from the index for a given name.
	// Objects returned here must be treated as read-only.
	Get(name string) (*v1alpha1.MachineOSConfig, error)
	MachineOSConfigListerExpansion
}

// machineOSConfigLister implements the MachineOSConfigLister interface.
type machineOSConfigLister struct {
	indexer cache.Indexer
}

// NewMachineOSConfigLister returns a new MachineOSConfigLister.
func NewMachineOSConfigLister(indexer cache.Indexer) MachineOSConfigLister {
	return &machineOSConfigLister{indexer: indexer}
}

// List lists all MachineOSConfigs in the indexer.
func (s *machineOSConfigLister) List(selector labels.Selector) (ret []*v1alpha1.MachineOSConfig, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1alpha1.MachineOSConfig))
	})
	return ret, err
}

// Get retrieves the MachineOSConfig from the index for a given name.
func (s *machineOSConfigLister) Get(name string) (*v1alpha1.MachineOSConfig, error) {
	obj, exists, err := s.indexer.GetByKey(name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(v1alpha1.Resource("machineosconfig"), name)
	}
	return obj.(*v1alpha1.MachineOSConfig), nil
}
