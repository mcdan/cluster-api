/*
Copyright 2023 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package server

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/log"

	infrav1 "sigs.k8s.io/cluster-api/test/infrastructure/inmemory/api/v1alpha1"
	cmanager "sigs.k8s.io/cluster-api/test/infrastructure/inmemory/internal/cloud/runtime/manager"
	"sigs.k8s.io/cluster-api/test/infrastructure/inmemory/internal/server/api"
	"sigs.k8s.io/cluster-api/test/infrastructure/inmemory/internal/server/etcd"
	"sigs.k8s.io/cluster-api/util/certs"
)

const (
	debugPort = 19000

	// This range allows for 4k clusters, which is 4 times the goal we have in mind for
	// the first iteration of stress tests.

	minPort = 20000
	maxPort = 24000
)

// WorkloadClustersMux implements a server that handles requests for multiple workload clusters.
// Each workload clusters will get its own listener, serving on a dedicated port, eg.
// wkl-cluster-1 >> :20000, wkl-cluster-2 >> :20001 etc.
// Each workload cluster will act both as API server and as etcd for the cluster; the
// WorkloadClustersMux is also responsible for handling certificates for each of the above use cases.
type WorkloadClustersMux struct {
	host      string
	minPort   int // TODO: move port management to a port range type
	maxPort   int
	portIndex int

	manager cmanager.Manager // TODO: figure out if we can have a smaller interface (GetResourceGroup, GetSchema)

	debugServer              http.Server
	muxServer                http.Server
	workloadClusterListeners map[string]*WorkloadClusterListener
	// workloadClusterNameByHost maps from Host to workload cluster name.
	workloadClusterNameByHost map[string]string

	lock sync.RWMutex
	log  logr.Logger
}

// NewWorkloadClustersMux returns a WorkloadClustersMux that handles requests for multiple workload clusters.
func NewWorkloadClustersMux(manager cmanager.Manager, host string) *WorkloadClustersMux {
	m := &WorkloadClustersMux{
		host:                      host,
		minPort:                   minPort,
		maxPort:                   maxPort,
		portIndex:                 minPort,
		manager:                   manager,
		workloadClusterListeners:  map[string]*WorkloadClusterListener{},
		workloadClusterNameByHost: map[string]string{},
		log:                       log.Log,
	}

	//nolint:gosec // Ignoring the following for now: "G112: Potential Slowloris Attack because ReadHeaderTimeout is not configured in the http.Server (gosec)"
	m.muxServer = http.Server{
		// Use an handler that can serve either API server calls or etcd calls.
		Handler: m.mixedHandler(),
		// Use a TLS config that selects certificates for a specific cluster depending on
		// the request being processed (API server and etcd have different certificates).
		TLSConfig: &tls.Config{
			GetCertificate: func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
				return m.getCertificate(info)
			},
			MinVersion: tls.VersionTLS12,
		},
	}

	//nolint:gosec // Ignoring the following for now: "G112: Potential Slowloris Attack because ReadHeaderTimeout is not configured in the http.Server (gosec)"
	m.debugServer = http.Server{
		Handler: api.NewDebugHandler(manager, m.log, m),
	}
	l, _ := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", debugPort)))
	go func() { _ = m.debugServer.Serve(l) }()

	return m
}

// mixedHandler returns an handler that can serve either API server calls or etcd calls.
func (m *WorkloadClustersMux) mixedHandler() http.Handler {
	// Prepare a function that can identify which workloadCluster/resourceGroup a
	// request targets to.
	// IMPORTANT: this function assumes that both the listener and the resourceGroup
	// for a workload cluster have the same name.
	resourceGroupResolver := func(host string) (string, error) {
		m.lock.RLock()
		defer m.lock.RUnlock()
		wclName, ok := m.workloadClusterNameByHost[host]
		if !ok {
			return "", errors.Errorf("failed to get workloadClusterListener for host %s", host)
		}
		return wclName, nil
	}

	// build the handlers for API server and etcd.
	apiHandler := api.NewAPIServerHandler(m.manager, m.log, resourceGroupResolver)
	etcdHandler := etcd.NewEtcdServerHandler(m.manager, m.log, resourceGroupResolver)

	// Creates the mixed handler combining the two above depending on
	// the type of request being processed
	mixedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("content-type"), "application/grpc") {
			etcdHandler.ServeHTTP(w, r)
			return
		}
		apiHandler.ServeHTTP(w, r)
	})

	return h2c.NewHandler(mixedHandler, &http2.Server{})
}

// getCertificate selects certificates for a specific cluster depending on the request being processed
// (API server and etcd have different certificates).
func (m *WorkloadClustersMux) getCertificate(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.lock.RLock()
	defer m.lock.RUnlock()

	// Identify which workloadCluster/resourceGroup a request targets to.
	hostPort := info.Conn.LocalAddr().String()
	wclName, ok := m.workloadClusterNameByHost[hostPort]
	if !ok {
		err := errors.Errorf("failed to get listener name for workload cluster serving on %s", hostPort)
		m.log.Error(err, "Error resolving certificates")
		return nil, err
	}

	// Gets the listener config for the target workloadCluster.
	wcl, ok := m.workloadClusterListeners[wclName]
	if !ok {
		err := errors.Errorf("failed to get listener with name %s for workload cluster serving on %s", wclName, hostPort)
		m.log.Error(err, "Error resolving certificates")
		return nil, err
	}

	// If the request targets a specific etcd member, use the corresponding server certificates
	// NOTE: the port forward call to etcd sets the server name to the name of the targeted etcd pod,
	// which is also the name of the corresponding etcd member.
	if wcl.etcdMembers.Has(info.ServerName) {
		m.log.V(4).Info("Using etcd serving certificate", "listenerName", wcl, "host", hostPort, "etcdPod", info.ServerName)
		return wcl.etcdServingCertificates[info.ServerName], nil
	}

	// Otherwise we assume the request targets the API server.
	m.log.V(4).Info("Using API server serving certificate", "listenerName", wcl, "host", hostPort)
	return wcl.apiServerServingCertificate, nil
}

// HotRestart tries to set up the mux according to an existing set of InMemoryClusters.
// NOTE: This is done at best effort in order to make iterative development workflows easier.
func (m *WorkloadClustersMux) HotRestart(clusters *infrav1.InMemoryClusterList) error {
	if len(clusters.Items) == 0 {
		return nil
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	if len(m.workloadClusterListeners) > 0 {
		return errors.New("WorkloadClustersMux cannot be hot restarted when there are already initialized listeners")
	}

	ports := sets.Set[int]{}
	maxPort := m.minPort - 1
	for _, c := range clusters.Items {
		if c.Spec.ControlPlaneEndpoint.Host == "" {
			continue
		}

		if c.Spec.ControlPlaneEndpoint.Host != m.host {
			return errors.Errorf("unable to restart the WorkloadClustersMux, the host address is changed from %s to %s", c.Spec.ControlPlaneEndpoint.Host, m.host)
		}

		if ports.Has(c.Spec.ControlPlaneEndpoint.Port) {
			return errors.Errorf("unable to restart the WorkloadClustersMux, there are two or more clusters using port %d", c.Spec.ControlPlaneEndpoint.Port)
		}

		resourceGroup, ok := c.Annotations[infrav1.ResourceGroupAnnotationName]
		if !ok {
			return errors.Errorf("unable to restart the WorkloadClustersMux, cluster %s doesn't have the %s annotation", klog.KRef(c.Namespace, c.Name), infrav1.ResourceGroupAnnotationName)
		}

		m.initWorkloadClusterListenerWithPortLocked(resourceGroup, c.Spec.ControlPlaneEndpoint.Port)

		if maxPort < c.Spec.ControlPlaneEndpoint.Port {
			maxPort = c.Spec.ControlPlaneEndpoint.Port
		}
	}

	m.portIndex = maxPort + 1
	return nil
}

// InitWorkloadClusterListener initialize a WorkloadClusterListener by reserving a port for it.
// Note: The listener will be started when the first API server will be added.
func (m *WorkloadClustersMux) InitWorkloadClusterListener(wclName string) (*WorkloadClusterListener, error) {
	m.lock.Lock()
	defer m.lock.Unlock()

	if wcl, ok := m.workloadClusterListeners[wclName]; ok {
		return wcl, nil
	}

	port, err := m.getFreePortLocked()
	if err != nil {
		return nil, err
	}

	wcl := m.initWorkloadClusterListenerWithPortLocked(wclName, port)

	return wcl, nil
}

// initWorkloadClusterListenerWithPortLocked initializes a workload cluster listener.
// Note: m.lock must be locked before calling this method.
func (m *WorkloadClustersMux) initWorkloadClusterListenerWithPortLocked(wclName string, port int) *WorkloadClusterListener {
	wcl := &WorkloadClusterListener{
		scheme:                  m.manager.GetScheme(),
		host:                    m.host,
		port:                    port,
		apiServers:              sets.New[string](),
		etcdMembers:             sets.New[string](),
		etcdServingCertificates: map[string]*tls.Certificate{},
	}
	m.workloadClusterListeners[wclName] = wcl
	m.workloadClusterNameByHost[wcl.HostPort()] = wclName

	m.log.Info("Workload cluster listener created", "listenerName", wclName, "address", wcl.Address())
	return wcl
}

// AddAPIServer mimics adding an API server instance behind the WorkloadClusterListener.
// When the first API server instance is added the serving certificates and the admin certificate
// for tests are generated, and the listener is started.
func (m *WorkloadClustersMux) AddAPIServer(wclName, podName string, caCert *x509.Certificate, caKey *rsa.PrivateKey) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	wcl, ok := m.workloadClusterListeners[wclName]
	if !ok {
		return errors.Errorf("workloadClusterListener with name %s must be initialized before adding an APIserver", wclName)
	}
	wcl.apiServers.Insert(podName)
	m.log.Info("APIServer instance added to workloadClusterListener", "listenerName", wclName, "address", wcl.Address(), "podName", podName)

	// TODO: check if cert/key are already set, they should match
	wcl.apiServerCaCertificate = caCert
	wcl.apiServerCaKey = caKey

	// Generate Serving certificates for the API server instance
	// NOTE: There is only one server certificate for all API server instances (kubeadm
	// instead creates one for each API server pod). We don't need this because we are
	// accessing all API servers via the same endpoint.
	if wcl.apiServerServingCertificate == nil {
		config := apiServerCertificateConfig(wcl.host)
		cert, key, err := newCertAndKey(caCert, caKey, config)
		if err != nil {
			return errors.Wrapf(err, "failed to create serving certificate for API server %s", podName)
		}

		certificate, err := tls.X509KeyPair(certs.EncodeCertPEM(cert), certs.EncodePrivateKeyPEM(key))
		if err != nil {
			return errors.Wrapf(err, "failed to create X509KeyPair for API server %s", podName)
		}
		wcl.apiServerServingCertificate = &certificate
	}

	// Generate admin certificates to be used for accessing the API server.
	// NOTE: this is used for tests because CAPI creates its own.
	if wcl.adminCertificate == nil {
		config := adminClientCertificateConfig()
		cert, key, err := newCertAndKey(caCert, caKey, config)
		if err != nil {
			return errors.Wrapf(err, "failed to create admin certificate for API server %s", podName)
		}

		wcl.adminCertificate = cert
		wcl.adminKey = key
	}

	// Start the listener for the API server.
	// NOTE: There is only one listener for all API server instances; the same listener will act
	// as a port forward target too.
	if wcl.listener != nil {
		return nil
	}

	l, err := net.Listen("tcp", wcl.HostPort())
	if err != nil {
		return errors.Wrapf(err, "failed to start WorkloadClusterListener %s, %s", wclName, wcl.HostPort())
	}
	wcl.listener = l

	var startErr error
	startCh := make(chan struct{})
	go func() {
		startCh <- struct{}{}
		if err := m.muxServer.ServeTLS(wcl.listener, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			startErr = err
			m.log.Error(startErr, "Failed to start WorkloadClusterListener", "listenerName", wclName, "address", wcl.Address())
		}
	}()

	<-startCh
	// TODO: Try to make this race condition free e.g. by checking the listener is answering.
	// There is no guarantee ServeTLS was called after we received something on the startCh.
	time.Sleep(100 * time.Millisecond)

	if startErr != nil {
		return startErr
	}

	m.log.Info("WorkloadClusterListener successfully started", "listenerName", wclName, "address", wcl.Address())
	return nil
}

// HasAPIServer returns true if the workload cluster already has an apiserver with podName.
func (m *WorkloadClustersMux) HasAPIServer(wclName, podName string) bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	wcl, ok := m.workloadClusterListeners[wclName]
	if !ok {
		return false
	}
	return wcl.apiServers.Has(podName)
}

// AddEtcdMember mimics adding an etcd Member behind the WorkloadClusterListener;
// every etcd member gets a dedicated serving certificate, so it will be possible to serve port forward requests
// to a specific etcd pod/member.
func (m *WorkloadClustersMux) AddEtcdMember(wclName, podName string, caCert *x509.Certificate, caKey *rsa.PrivateKey) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	wcl, ok := m.workloadClusterListeners[wclName]
	if !ok {
		return errors.Errorf("workloadClusterListener with name %s must be initialized before adding an APIserver", wclName)
	}
	wcl.etcdMembers.Insert(podName)
	m.log.Info("Etcd member added to WorkloadClusterListener", "listenerName", wclName, "address", wcl.Address(), "podName", podName)

	// Generate Serving certificates for the etcdMember
	if _, ok := wcl.etcdServingCertificates[podName]; !ok {
		config := etcdServerCertificateConfig(podName, wcl.host)
		cert, key, err := newCertAndKey(caCert, caKey, config)
		if err != nil {
			return errors.Wrapf(err, "failed to create serving certificate for etcd member %s", podName)
		}

		certificate, err := tls.X509KeyPair(certs.EncodeCertPEM(cert), certs.EncodePrivateKeyPEM(key))
		if err != nil {
			return errors.Wrapf(err, "failed to create X509KeyPair for etcd member %s", podName)
		}
		wcl.etcdServingCertificates[podName] = &certificate
	}

	return nil
}

// HasEtcdMember returns true if the workload cluster already has an etcd member with podName.
func (m *WorkloadClustersMux) HasEtcdMember(wclName, podName string) bool {
	m.lock.RLock()
	defer m.lock.RUnlock()

	wcl, ok := m.workloadClusterListeners[wclName]
	if !ok {
		return false
	}
	return wcl.etcdMembers.Has(podName)
}

// ListListeners implements api.DebugInfoProvider.
func (m *WorkloadClustersMux) ListListeners() map[string]string {
	m.lock.RLock()
	defer m.lock.RUnlock()

	ret := map[string]string{}
	for k, l := range m.workloadClusterListeners {
		ret[k] = l.Address()
	}
	return ret
}

// Shutdown shuts down the workload cluster mux.
func (m *WorkloadClustersMux) Shutdown(ctx context.Context) error {
	m.lock.Lock()
	defer m.lock.Unlock()

	if err := m.debugServer.Shutdown(ctx); err != nil {
		return errors.Wrap(err, "failed to shutdown the debug server")
	}

	// NOTE: this closes all the listeners
	if err := m.muxServer.Shutdown(ctx); err != nil {
		return errors.Wrap(err, "failed to shutdown the mux server")
	}

	return nil
}

// getFreePortLocked gets a free port.
// Note: m.lock must be locked before calling this method.
func (m *WorkloadClustersMux) getFreePortLocked() (int, error) {
	port := m.portIndex
	if port > m.maxPort {
		return -1, errors.Errorf("no more free ports in the %d-%d range", m.minPort, m.maxPort)
	}

	// TODO: check the port is actually free. If not try the next one

	m.portIndex++
	return port, nil
}
