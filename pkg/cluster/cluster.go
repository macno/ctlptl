package cluster

import (
	"context"
	"sync"

	"github.com/tilt-dev/ctlptl/pkg/api"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog/v2"

	// Client auth plugins! They will auto-init if we import them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

type Controller struct {
	config  clientcmdapi.Config
	clients map[string]kubernetes.Interface
	mu      sync.Mutex
}

func ControllerWithConfig(config clientcmdapi.Config) *Controller {
	return &Controller{
		config:  config,
		clients: make(map[string]kubernetes.Interface),
	}
}

func DefaultController() (*Controller, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.DefaultClientConfig = &clientcmd.DefaultClientConfig

	overrides := &clientcmd.ConfigOverrides{}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	rawConfig, err := loader.RawConfig()
	if err != nil {
		return nil, err
	}
	return ControllerWithConfig(rawConfig), nil
}

func (c *Controller) client(name string) (kubernetes.Interface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	client, ok := c.clients[name]
	if ok {
		return client, nil
	}

	restConfig, err := clientcmd.NewDefaultClientConfig(
		c.config, &clientcmd.ConfigOverrides{CurrentContext: name}).ClientConfig()
	if err != nil {
		return nil, err
	}

	client, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	c.clients[name] = client
	return client, nil
}

func (c *Controller) populateCluster(ctx context.Context, cluster *api.Cluster) error {
	client, err := c.client(cluster.Name)
	if err != nil {
		return err
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	minTime := metav1.Time{}
	for _, node := range nodes.Items {
		cTime := node.CreationTimestamp
		if minTime.Time.IsZero() || cTime.Time.Before(minTime.Time) {
			minTime = cTime
		}
	}

	cluster.Status.CreationTimestamp = minTime
	return nil
}

func (c *Controller) List(ctx context.Context) ([]*api.Cluster, error) {
	result := []*api.Cluster{}
	for name, ct := range c.config.Contexts {
		cluster := &api.Cluster{
			TypeMeta: api.TypeMeta{APIVersion: "ctlptl.dev/v1alpha1", Kind: "Cluster"},
			Name:     name,
			Product:  productFromContext(ct).String(),
		}
		result = append(result, cluster)

		err := c.populateCluster(ctx, cluster)
		if err != nil {
			klog.V(4).Infof("WARNING: reading info off cluster %s: %v", name, err)
		}
	}
	return result, nil
}
