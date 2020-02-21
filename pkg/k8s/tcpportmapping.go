package tcpportmapping

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
)

/**
TODO:
- Cleanup the client for unnecessary functions
- Find a better name for this package
- Integrate this in the controller
- Make sure int32 is not too constraining once integrated
*/

// ServiceWithPort holds a combination of service name, namespace and port.
type ServiceWithPort struct {
	Namespace string
	Name      string
	Port      int32
}

// TCPPortMapper is capable of storing and retrieving a TCP port mapping for a given service.
type TCPPortMapper interface {
	Find(svc ServiceWithPort) (int32, bool)
	Get(srcPort int32) *ServiceWithPort
	Add(svc *ServiceWithPort) (int32, error)
}

// TCPPortMapping is a TCPPortMapper backed by a Kubernetes ConfigMap.
type TCPPortMapping struct {
	mu    sync.RWMutex
	table map[int32]*ServiceWithPort

	minPort int32
	maxPort int32

	client          kubernetes.Interface
	lister          listers.ConfigMapLister
	cfgMapNamespace string
	cfgMapName      string
}

// NewTCPPortMapping creates a new TCPPortMapping instance.
func NewTCPPortMapping(client kubernetes.Interface, l listers.ConfigMapLister, cfgMapNamespace, cfgMapName string, minPort, maxPort int32) (*TCPPortMapping, error) {
	m := &TCPPortMapping{
		minPort:         minPort,
		maxPort:         maxPort,
		table:           make(map[int32]*ServiceWithPort),
		client:          client,
		lister:          l,
		cfgMapNamespace: cfgMapNamespace,
		cfgMapName:      cfgMapName,
	}

	if err := m.loadState(); err != nil {
		return nil, err
	}

	return m, nil
}

// Find searches for the port which is associated with the given ServiceWithPort.
func (m *TCPPortMapping) Find(svc ServiceWithPort) (int32, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for port, v := range m.table {
		if v.Name == svc.Name && v.Namespace == svc.Namespace && v.Port == svc.Port {
			return port, true
		}
	}
	return 0, false
}

// Get returns the SearviceWithPort associated to the given port.
func (m *TCPPortMapping) Get(srcPort int32) *ServiceWithPort {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.table[srcPort]
}

// Add adds a new mapping between the given ServiceWithPort and the first port available in the range defined
// within minPort and maxPort. If there's no port left, an error will be returned.
func (m *TCPPortMapping) Add(svc *ServiceWithPort) (int32, error) {
	for i := m.minPort; i < m.maxPort+1; i++ {
		// Skip until an available port is found
		if _, exists := m.table[i]; exists {
			continue
		}

		m.mu.Lock()
		m.table[i] = svc
		m.mu.Unlock()

		if err := m.saveState(); err != nil {
			// If the state can't be saved, we are going to have a mismatch between the local table and the ConfigMap.
			// By not undoing the assignment on the local table we allow the state to converge in the  future calls to
			// Add, making it more robust to temporary failure.
			return 0, fmt.Errorf("unable to save TCP port mapping: %w", err)
		}

		return i, nil
	}

	return 0, errors.New("unable to find an available port")
}

func (m *TCPPortMapping) loadState() error {
	cfg, err := m.lister.ConfigMaps(m.cfgMapNamespace).Get(m.cfgMapName)
	if err != nil {
		return fmt.Errorf("unable to load state from ConfigMap %q in namespace %q: %w", m.cfgMapName, m.cfgMapNamespace, err)
	}

	if len(cfg.Data) > 0 {
		m.mu.Lock()
		defer m.mu.Unlock()

		for k, v := range cfg.Data {
			port, err := strconv.ParseInt(k, 10, 32)
			if err != nil {
				continue
			}

			svc, err := parseServiceNamePort(v)
			if err != nil {
				continue
			}

			m.table[int32(port)] = svc
		}
	}

	return nil
}

func (m *TCPPortMapping) saveState() error {
	cfg, err := m.lister.ConfigMaps(m.cfgMapNamespace).Get(m.cfgMapName)
	if err != nil {
		return err
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cpy := cfg.DeepCopy()

		if cpy.Data == nil {
			cpy.Data = make(map[string]string)
		}

		m.mu.RLock()
		for k, v := range m.table {
			key := strconv.Itoa(int(k))
			value := formatServiceNamePort(v.Name, v.Namespace, v.Port)
			cpy.Data[key] = value
		}
		m.mu.RUnlock()

		_, err := m.client.CoreV1().ConfigMaps(cfg.Namespace).Update(cpy)
		return err
	})
}

func parseServiceNamePort(value string) (*ServiceWithPort, error) {
	service := strings.Split(value, ":")
	if len(service) < 2 {
		return nil, fmt.Errorf("could not parse service into name and port")
	}

	port64, err := strconv.ParseInt(service[1], 10, 32)
	if err != nil {
		return nil, err
	}

	substring := strings.Split(service[0], "/")

	// TODO: Remove this if we can't have such pattern in the configMap.
	if len(substring) == 1 {
		return &ServiceWithPort{
			Name:      substring[0],
			Namespace: metav1.NamespaceDefault,
			Port:      int32(port64),
		}, nil
	}

	return &ServiceWithPort{
		Name:      substring[1],
		Namespace: substring[0],
		Port:      int32(port64),
	}, nil
}

func formatServiceNamePort(name, namespace string, port int32) (value string) {
	return fmt.Sprintf("%s/%s:%d", namespace, name, port)
}
