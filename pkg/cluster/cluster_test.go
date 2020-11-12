package cluster

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tilt-dev/ctlptl/pkg/api"
	"github.com/tilt-dev/ctlptl/pkg/registry"
	"github.com/tilt-dev/localregistry-go"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestClusterGet(t *testing.T) {
	c := newFakeController(t)
	cluster, err := c.Get(context.Background(), "microk8s")
	assert.NoError(t, err)
	assert.Equal(t, cluster.Name, "microk8s")
	assert.Equal(t, cluster.Product, "microk8s")
}

func TestClusterList(t *testing.T) {
	c := newFakeController(t)
	clusters, err := c.List(context.Background(), ListOptions{})
	assert.NoError(t, err)
	require.Equal(t, 2, len(clusters.Items))
	assert.Equal(t, "docker-desktop", clusters.Items[0].Name)
	assert.Equal(t, "microk8s", clusters.Items[1].Name)
}

func TestClusterListSelectorMatch(t *testing.T) {
	c := newFakeController(t)
	clusters, err := c.List(context.Background(), ListOptions{FieldSelector: "product=microk8s"})
	assert.NoError(t, err)
	require.Equal(t, 1, len(clusters.Items))
	assert.Equal(t, "microk8s", clusters.Items[0].Name)
}

func TestClusterListSelectorNoMatch(t *testing.T) {
	c := newFakeController(t)
	clusters, err := c.List(context.Background(), ListOptions{FieldSelector: "product=kind"})
	assert.NoError(t, err)
	assert.Equal(t, 0, len(clusters.Items))
}

func TestClusterGetMissing(t *testing.T) {
	c := newFakeController(t)
	_, err := c.Get(context.Background(), "dunkees")
	if assert.Error(t, err) {
		assert.True(t, errors.IsNotFound(err))
	}
}

func TestClusterApplyKIND(t *testing.T) {
	f := newFixture(t)
	f.dmachine.os = "darwin"

	assert.Equal(t, false, f.d4m.started)
	kindAdmin := f.newFakeAdmin(ProductKIND)

	result, err := f.controller.Apply(context.Background(), &api.Cluster{
		Product: string(ProductKIND),
	})
	assert.NoError(t, err)
	assert.Equal(t, true, f.d4m.started)
	assert.Equal(t, "kind-kind", kindAdmin.created.Name)
	assert.Equal(t, "kind-kind", result.Name)
}

func TestClusterApplyKINDWithCluster(t *testing.T) {
	f := newFixture(t)
	f.dmachine.os = "linux"

	f.dockerClient.started = true

	kindAdmin := f.newFakeAdmin(ProductKIND)

	result, err := f.controller.Apply(context.Background(), &api.Cluster{
		Product:  string(ProductKIND),
		Registry: "kind-registry",
	})
	assert.NoError(t, err)
	assert.Equal(t, "kind-kind", result.Name)
	assert.Equal(t, "kind-registry", result.Registry)
	assert.Equal(t, "kind-registry", kindAdmin.createdRegistry.Name)
	assert.Equal(t, 5000, kindAdmin.createdRegistry.Status.ContainerPort)
	assert.Equal(t, "kind-registry", f.registryCtl.lastApply.Name)
}

func TestClusterApplyDockerDesktop(t *testing.T) {
	f := newFixture(t)
	f.dmachine.os = "darwin"

	assert.Equal(t, false, f.d4m.started)
	assert.Equal(t, 1, f.dockerClient.ncpu)
	f.controller.Apply(context.Background(), &api.Cluster{
		Product: string(ProductDockerDesktop),
		MinCPUs: 3,
	})
	assert.Equal(t, true, f.d4m.started)
	assert.Equal(t, 3, f.dockerClient.ncpu)
}

func TestClusterApplyDockerDesktopCPUOnly(t *testing.T) {
	f := newFixture(t)
	f.dmachine.os = "darwin"

	err := f.d4m.start(context.Background())
	require.NoError(t, err)

	assert.Equal(t, true, f.d4m.started)
	assert.Equal(t, 1, f.dockerClient.ncpu)
	f.controller.Apply(context.Background(), &api.Cluster{
		Product: string(ProductDockerDesktop),
		MinCPUs: 3,
	})
	assert.Equal(t, true, f.d4m.started)
	assert.Equal(t, 3, f.dockerClient.ncpu)
}

func TestClusterApplyDockerDesktopStartClusterOnly(t *testing.T) {
	f := newFixture(t)
	f.dmachine.os = "darwin"

	assert.Equal(t, false, f.d4m.started)
	assert.Equal(t, 1, f.dockerClient.ncpu)
	f.controller.Apply(context.Background(), &api.Cluster{
		Product: string(ProductDockerDesktop),
		MinCPUs: 0,
	})
	assert.Equal(t, true, f.d4m.started)
	assert.Equal(t, 1, f.dockerClient.ncpu)
}

func TestClusterApplyDockerDesktopNoRestart(t *testing.T) {
	f := newFixture(t)
	f.dmachine.os = "darwin"

	assert.Equal(t, 0, f.d4m.settingsWriteCount)

	// Pretend the cluster isn't running.
	err := f.fakeK8s.Tracker().Delete(schema.GroupVersionResource{"", "v1", "nodes"}, "", "node-1")
	assert.NoError(t, err)

	f.controller.Apply(context.Background(), &api.Cluster{
		Product: string(ProductDockerDesktop),
	})
	assert.Equal(t, 1, f.d4m.settingsWriteCount)

	f.controller.Apply(context.Background(), &api.Cluster{
		Product: string(ProductDockerDesktop),
	})
	assert.Equal(t, 1, f.d4m.settingsWriteCount)
}

func TestClusterApplyMinikubeVersion(t *testing.T) {
	f := newFixture(t)
	f.dmachine.os = "darwin"

	assert.Equal(t, false, f.d4m.started)
	minikubeAdmin := f.newFakeAdmin(ProductMinikube)

	result, err := f.controller.Apply(context.Background(), &api.Cluster{
		Product:           string(ProductMinikube),
		KubernetesVersion: "v1.14.0",
	})
	assert.NoError(t, err)
	assert.Equal(t, true, f.d4m.started)
	assert.Equal(t, "minikube", minikubeAdmin.created.Name)
	assert.Equal(t, "minikube", result.Name)

	minikubeAdmin.created = nil

	_, err = f.controller.Apply(context.Background(), &api.Cluster{
		Product:           string(ProductMinikube),
		KubernetesVersion: "v1.14.0",
	})
	assert.NoError(t, err)

	// Make sure we don't recreate the cluster.
	assert.Nil(t, minikubeAdmin.created)

	// Now, change the version and make sure we re-create the cluster.
	out := bytes.NewBuffer(nil)
	f.controller.iostreams.ErrOut = out

	_, err = f.controller.Apply(context.Background(), &api.Cluster{
		Product:           string(ProductMinikube),
		KubernetesVersion: "v1.15.0",
	})
	assert.NoError(t, err)

	assert.Equal(t, "minikube", minikubeAdmin.created.Name)
	assert.Contains(t, out.String(),
		"Deleting cluster minikube because desired Kubernetes version (v1.15.0) "+
			"does not match current (v1.14.0)")
}

type fixture struct {
	t            *testing.T
	controller   *Controller
	dockerClient *fakeDockerClient
	dmachine     *dockerMachine
	d4m          *fakeD4MClient
	config       *clientcmdapi.Config
	registryCtl  *fakeRegistryController
	fakeK8s      *fake.Clientset
}

func newFixture(t *testing.T) *fixture {
	dockerClient := &fakeDockerClient{ncpu: 1}
	d4m := &fakeD4MClient{docker: dockerClient}
	dmachine := &dockerMachine{
		dockerClient: dockerClient,
		errOut:       os.Stderr,
		sleep:        func(d time.Duration) {},
		d4m:          d4m,
		os:           "darwin", // default to macos
	}
	config := &clientcmdapi.Config{
		CurrentContext: "microk8s",
		Contexts: map[string]*clientcmdapi.Context{
			"microk8s": &clientcmdapi.Context{
				Cluster: "microk8s-cluster",
			},
			"docker-desktop": &clientcmdapi.Context{
				Cluster: "docker-desktop",
			},
		},
		Clusters: map[string]*clientcmdapi.Cluster{
			"microk8s-cluster": &clientcmdapi.Cluster{Server: "http://microk8s.localhost/"},
			"docker-desktop":   &clientcmdapi.Cluster{Server: "http://docker-desktop.localhost/"},
		},
	}
	configLoader := configLoader(func() (clientcmdapi.Config, error) {
		return *config, nil
	})
	iostreams := genericclioptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-1",
			CreationTimestamp: metav1.Time{Time: time.Now()},
		},
	}
	fakeK8s := fake.NewSimpleClientset(node)
	clientLoader := clientLoader(func(restConfig *rest.Config) (kubernetes.Interface, error) {
		return fakeK8s, nil
	})

	registryCtl := &fakeRegistryController{}
	controller := &Controller{
		iostreams:    iostreams,
		admins:       make(map[Product]Admin),
		config:       *config,
		dmachine:     dmachine,
		configLoader: configLoader,
		clientLoader: clientLoader,
		clients:      make(map[string]kubernetes.Interface),
		registryCtl:  registryCtl,
	}
	return &fixture{
		t:            t,
		controller:   controller,
		dmachine:     dmachine,
		d4m:          d4m,
		dockerClient: dockerClient,
		config:       config,
		registryCtl:  registryCtl,
		fakeK8s:      fakeK8s,
	}
}

func (f *fixture) newFakeAdmin(p Product) *fakeAdmin {
	admin := newFakeAdmin(f.config, f.fakeK8s)
	f.controller.admins[p] = admin
	return admin
}

func newFakeController(t *testing.T) *Controller {
	return newFixture(t).controller
}

type fakeDockerClient struct {
	started bool
	ncpu    int
}

func (c *fakeDockerClient) ServerVersion(ctx context.Context) (types.Version, error) {
	if !c.started {
		return types.Version{}, fmt.Errorf("not started")
	}

	return types.Version{}, nil
}

func (c *fakeDockerClient) Info(ctx context.Context) (types.Info, error) {
	if !c.started {
		return types.Info{}, fmt.Errorf("not started")
	}

	return types.Info{NCPU: c.ncpu}, nil
}

type fakeD4MClient struct {
	lastSettings       map[string]interface{}
	docker             *fakeDockerClient
	started            bool
	settingsWriteCount int
	resetCount         int
}

func (c *fakeD4MClient) writeSettings(ctx context.Context, settings map[string]interface{}) error {
	c.lastSettings = settings
	c.docker.ncpu = settings["cpu"].(int)
	c.settingsWriteCount++
	return nil
}

func (c *fakeD4MClient) settings(ctx context.Context) (map[string]interface{}, error) {
	return c.lastSettings, nil
}

func (c *fakeD4MClient) setK8sEnabled(settings map[string]interface{}, desired bool) (bool, error) {
	enabled, ok := settings["k8sEnabled"]
	if ok && enabled.(bool) == true {
		return false, nil
	}
	settings["k8sEnabled"] = true
	return true, nil
}

func (c *fakeD4MClient) ensureMinCPU(settings map[string]interface{}, desired int) (bool, error) {
	cpu, ok := settings["cpu"]
	if ok && cpu.(int) >= desired {
		return false, nil
	}
	settings["cpu"] = desired
	return true, nil

}

func (c *fakeD4MClient) resetK8s(ctx context.Context) error {
	c.resetCount++
	return nil
}

func (c *fakeD4MClient) start(ctx context.Context) error {
	c.lastSettings = map[string]interface{}{
		"cpu": c.docker.ncpu,
	}
	c.started = true
	c.docker.started = true
	return nil
}

type fakeAdmin struct {
	created         *api.Cluster
	createdRegistry *api.Registry
	deleted         *api.Cluster
	config          *clientcmdapi.Config
	fakeK8s         *fake.Clientset
	serverVersion   *version.Info
}

func newFakeAdmin(config *clientcmdapi.Config, fakeK8s *fake.Clientset) *fakeAdmin {
	return &fakeAdmin{config: config, fakeK8s: fakeK8s}
}

func (a *fakeAdmin) EnsureInstalled(ctx context.Context) error { return nil }

func (a *fakeAdmin) Create(ctx context.Context, config *api.Cluster, registry *api.Registry) error {
	a.created = config.DeepCopy()
	a.createdRegistry = registry.DeepCopy()
	a.config.Contexts[config.Name] = &clientcmdapi.Context{Cluster: config.Name}
	a.config.Clusters[config.Name] = &clientcmdapi.Cluster{Server: fmt.Sprintf("http://%s.localhost/", config.Name)}

	kVersion := config.KubernetesVersion
	if kVersion == "" {
		kVersion = "v1.19.1"
	}
	a.fakeK8s.Discovery().(*discoveryfake.FakeDiscovery).FakedServerVersion = &version.Info{
		GitVersion: kVersion,
	}
	return nil
}

func (a *fakeAdmin) LocalRegistryHosting(registry *api.Registry) *localregistry.LocalRegistryHostingV1 {
	return &localregistry.LocalRegistryHostingV1{
		Host: fmt.Sprintf("localhost:%d", registry.Status.HostPort),
		Help: "https://github.com/tilt-dev/ctlptl",
	}
}

func (a *fakeAdmin) Delete(ctx context.Context, config *api.Cluster) error {
	a.deleted = config.DeepCopy()
	delete(a.config.Contexts, config.Name)
	return nil
}

type fakeRegistryController struct {
	lastApply *api.Registry
}

func (c *fakeRegistryController) List(ctx context.Context, options registry.ListOptions) (*api.RegistryList, error) {
	list := &api.RegistryList{}
	if c.lastApply != nil {
		item := c.lastApply.DeepCopy()
		list.Items = append(list.Items, *item)
	}
	return list, nil
}

func (c *fakeRegistryController) Apply(ctx context.Context, r *api.Registry) (*api.Registry, error) {
	c.lastApply = r.DeepCopy()

	newR := r.DeepCopy()
	newR.Status = api.RegistryStatus{
		ContainerPort: 5000,
		ContainerID:   "fake-container-id",
		HostPort:      5000,
		IPAddress:     "172.0.0.2",
		Networks:      []string{"bridge"},
	}
	return newR, nil
}
