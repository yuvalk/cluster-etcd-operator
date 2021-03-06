package hostendpointscontroller

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/openshift/cluster-etcd-operator/pkg/dnshelpers"

	operatorv1 "github.com/openshift/api/operator/v1"
	configv1informers "github.com/openshift/client-go/config/informers/externalversions/config/v1"
	configv1listers "github.com/openshift/client-go/config/listers/config/v1"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	"github.com/openshift/library-go/pkg/operator/resource/resourcemerge"
	"github.com/openshift/library-go/pkg/operator/v1helpers"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/mergepatch"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	"github.com/openshift/cluster-etcd-operator/pkg/operator/operatorclient"
)

const (
	workQueueKey = "key"
)

// HostEndpointsController maintains an Endpoints resource with
// the dns names of the current etcd cluster members for use by
// components unable to use the etcd service directly.
type HostEndpointsController struct {
	operatorClient       v1helpers.OperatorClient
	infrastructureLister configv1listers.InfrastructureLister
	networkLister        configv1listers.NetworkLister
	nodeLister           corev1listers.NodeLister
	endpointsLister      corev1listers.EndpointsLister
	endpointsClient      corev1client.EndpointsGetter

	eventRecorder events.Recorder
	queue         workqueue.RateLimitingInterface
	cachesToSync  []cache.InformerSynced
}

func NewHostEndpointsController(
	operatorClient v1helpers.OperatorClient,
	eventRecorder events.Recorder,
	kubeClient kubernetes.Interface,
	kubeInformers operatorv1helpers.KubeInformersForNamespaces,
	infrastructureInformer configv1informers.InfrastructureInformer,
	networkInformer configv1informers.NetworkInformer,
) *HostEndpointsController {
	kubeInformersForTargetNamespace := kubeInformers.InformersFor(operatorclient.TargetNamespace)
	endpointsInformer := kubeInformersForTargetNamespace.Core().V1().Endpoints()
	kubeInformersForCluster := kubeInformers.InformersFor("")
	nodeInformer := kubeInformersForCluster.Core().V1().Nodes()

	c := &HostEndpointsController{
		eventRecorder: eventRecorder.WithComponentSuffix("host-etcd-endpoints-controller"),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "HostEndpointsController"),
		cachesToSync: []cache.InformerSynced{
			operatorClient.Informer().HasSynced,
			endpointsInformer.Informer().HasSynced,
			nodeInformer.Informer().HasSynced,
			infrastructureInformer.Informer().HasSynced,
			networkInformer.Informer().HasSynced,
		},
		operatorClient:       operatorClient,
		infrastructureLister: infrastructureInformer.Lister(),
		networkLister:        networkInformer.Lister(),
		nodeLister:           nodeInformer.Lister(),
		endpointsLister:      endpointsInformer.Lister(),
		endpointsClient:      kubeClient.CoreV1(),
	}
	operatorClient.Informer().AddEventHandler(c.eventHandler())
	endpointsInformer.Informer().AddEventHandler(c.eventHandler())
	infrastructureInformer.Informer().AddEventHandler(c.eventHandler())
	networkInformer.Informer().AddEventHandler(c.eventHandler())
	nodeInformer.Informer().AddEventHandler(c.eventHandler())
	return c
}

func (c *HostEndpointsController) sync() error {
	err := c.syncHostEndpoints()

	if err != nil {
		_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
			Type:    "HostEndpointsDegraded",
			Status:  operatorv1.ConditionTrue,
			Reason:  "ErrorUpdatingHostEndpoints",
			Message: err.Error(),
		}))
		if updateErr != nil {
			c.eventRecorder.Warning("HostEndpointsErrorUpdatingStatus", updateErr.Error())
		}
		return err
	}

	_, _, updateErr := v1helpers.UpdateStatus(c.operatorClient, v1helpers.UpdateConditionFn(operatorv1.OperatorCondition{
		Type:   "HostEndpointsDegraded",
		Status: operatorv1.ConditionFalse,
		Reason: "HostEndpointsUpdated",
	}))
	if updateErr != nil {
		c.eventRecorder.Warning("HostEndpointsErrorUpdatingStatus", updateErr.Error())
		return updateErr
	}
	return nil
}

func (c *HostEndpointsController) syncHostEndpoints() error {
	// host-etc must exist in order to continue. we don't want to lose the etcd-bootstrap host.
	existing, err := c.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd")
	if err != nil {
		return err
	}

	required := hostEndpointsAsset()

	discoveryDomain, err := c.getEtcdDiscoveryDomain()
	if err != nil {
		return fmt.Errorf("unable to determine etcd discovery domain: %v", err)
	}

	if required.Annotations == nil {
		required.Annotations = map[string]string{}
	}
	required.Annotations["alpha.installer.openshift.io/dns-suffix"] = discoveryDomain

	// create endpoint addresses for each node
	network, err := c.networkLister.Get("cluster")
	if err != nil {
		return err
	}

	nodes, err := c.nodeLister.List(labels.Set{"node-role.kubernetes.io/master": ""}.AsSelector())
	if err != nil {
		return fmt.Errorf("unable to list expected etcd member nodes: %v", err)
	}
	endpointAddresses := []corev1.EndpointAddress{}
	for _, node := range nodes {
		nodeInternalIP, _, err := dnshelpers.GetPreferredInternalIPAddressForNodeName(network, node)
		if err != nil {
			return err
		}
		if len(nodeInternalIP) == 0 {
			return fmt.Errorf("unable to determine internal ip address for node %s", node.Name)
		}
		dnsName, err := c.getEtcdDNSName(discoveryDomain, nodeInternalIP)
		if err != nil {
			return fmt.Errorf("unable to determine etcd member dns name for node %s: %v", node.Name, err)
		}

		endpointAddresses = append(endpointAddresses, corev1.EndpointAddress{
			IP:       nodeInternalIP,
			Hostname: strings.TrimSuffix(dnsName, "."+discoveryDomain),
			NodeName: &node.Name,
		})
	}

	// if etcd-bootstrap exists, keep it
	for _, endpointAddress := range existing.Subsets[0].Addresses {
		if endpointAddress.Hostname == "etcd-bootstrap" {
			endpointAddresses = append(endpointAddresses, *endpointAddress.DeepCopy())
			break
		}
	}

	required.Subsets[0].Addresses = endpointAddresses
	if len(required.Subsets[0].Addresses) == 0 {
		return fmt.Errorf("no etcd member nodes are ready")
	}

	return c.applyEndpoints(required)
}

func hostEndpointsAsset() *corev1.Endpoints {
	return &corev1.Endpoints{
		ObjectMeta: v1.ObjectMeta{
			Name:      "host-etcd",
			Namespace: operatorclient.TargetNamespace,
		},
		Subsets: []corev1.EndpointSubset{
			{
				Ports: []corev1.EndpointPort{
					{
						Name:     "etcd",
						Port:     2379,
						Protocol: "TCP",
					},
				},
			},
		},
	}
}

func (c *HostEndpointsController) getEtcdDiscoveryDomain() (string, error) {
	infrastructure, err := c.infrastructureLister.Get("cluster")
	if err != nil {
		return "", err
	}
	etcdDiscoveryDomain := infrastructure.Status.EtcdDiscoveryDomain
	if len(etcdDiscoveryDomain) == 0 {
		return "", fmt.Errorf("infrastructures.config.openshit.io/cluster missing .status.etcdDiscoveryDomain")
	}
	return etcdDiscoveryDomain, nil
}

func (c *HostEndpointsController) getEtcdDNSName(discoveryDomain, ip string) (string, error) {
	dnsName, err := reverseLookup("etcd-server-ssl", "tcp", discoveryDomain, ip)
	if err != nil {
		return "", err
	}
	return dnsName, nil
}

// returns the target from the SRV record that resolves to ip.
func reverseLookup(service, proto, name, ip string) (string, error) {
	_, srvs, err := net.LookupSRV(service, proto, name)
	if err != nil {
		return "", err
	}
	selfTarget := ""
	for _, srv := range srvs {
		klog.V(4).Infof("checking against %s", srv.Target)
		addrs, err := net.LookupHost(srv.Target)
		if err != nil {
			return "", fmt.Errorf("could not resolve member %q", srv.Target)
		}

		for _, addr := range addrs {
			if addr == ip {
				selfTarget = strings.Trim(srv.Target, ".")
				break
			}
		}
	}
	if selfTarget == "" {
		return "", fmt.Errorf("could not find self")
	}
	return selfTarget, nil
}

func (c *HostEndpointsController) applyEndpoints(required *corev1.Endpoints) error {
	existing, err := c.endpointsLister.Endpoints(operatorclient.TargetNamespace).Get("host-etcd")
	if errors.IsNotFound(err) {
		_, err := c.endpointsClient.Endpoints(operatorclient.TargetNamespace).Create(required)
		if err != nil {
			c.eventRecorder.Warningf("EndpointsCreateFailed", "Failed to create endpoints/%s -n %s: %v", required.Name, required.Namespace, err)
			return err
		}
		c.eventRecorder.Warningf("EndpointsCreated", "Created endpoints/%s -n %s because it was missing", required.Name, required.Namespace)
	}
	if err != nil {
		return err
	}
	modified := resourcemerge.BoolPtr(false)
	toWrite := existing.DeepCopy()
	resourcemerge.EnsureObjectMeta(modified, &toWrite.ObjectMeta, required.ObjectMeta)
	if !endpointsSubsetsEqual(existing.Subsets, required.Subsets) {
		toWrite.Subsets = make([]corev1.EndpointSubset, len(required.Subsets))
		for i := range required.Subsets {
			required.Subsets[i].DeepCopyInto(&(toWrite.Subsets)[i])
		}
		*modified = true
	}
	if !*modified {
		// no update needed
		return nil
	}
	jsonPatch := resourceapply.JSONPatchNoError(existing, toWrite)
	if klog.V(4) {
		klog.Infof("Endpoints %q changes: %v", required.Namespace+"/"+required.Name, jsonPatch)
	}
	updated, err := c.endpointsClient.Endpoints(operatorclient.TargetNamespace).Update(toWrite)
	if err != nil {
		c.eventRecorder.Warningf("EndpointsUpdateFailed", "Failed to update endpoints/%s -n %s: %v", required.Name, required.Namespace, err)
		return err
	}
	klog.Infof("toWrite: \n%v", mergepatch.ToYAMLOrError(updated.Subsets))
	c.eventRecorder.Warningf("EndpointsUpdated", "Updated endpoints/%s -n %s because it changed: %v", required.Name, required.Namespace, jsonPatch)
	return nil
}

func endpointsSubsetsEqual(lhs, rhs []corev1.EndpointSubset) bool {
	if len(lhs) != len(rhs) {
		return false
	}
	for i := range lhs {
		if !endpointSubsetEqual(lhs[i], rhs[i]) {
			return false
		}
	}
	return true
}

func endpointSubsetEqual(lhs, rhs corev1.EndpointSubset) bool {
	if len(lhs.Addresses) != len(rhs.Addresses) {
		return false
	}
	if len(lhs.NotReadyAddresses) != len(rhs.NotReadyAddresses) {
		return false
	}
	if len(lhs.Ports) != len(rhs.Ports) {
		return false
	}
	// sorts the endpoint addresses for comparison (make copy as to not clobber originals)
	count := len(lhs.Addresses)
	lhsAddresses := make([]corev1.EndpointAddress, count)
	rhsAddresses := make([]corev1.EndpointAddress, count)
	for i := 0; i < count; i++ {
		lhs.Addresses[i].DeepCopyInto(&lhsAddresses[i])
		rhs.Addresses[i].DeepCopyInto(&rhsAddresses[i])
	}
	sort.Slice(lhsAddresses, newEndpointAddressSliceComparator(lhsAddresses))
	sort.Slice(rhsAddresses, newEndpointAddressSliceComparator(rhsAddresses))
	return reflect.DeepEqual(lhsAddresses, rhsAddresses)
}

func newEndpointAddressSliceComparator(endpointAddresses []corev1.EndpointAddress) func(int, int) bool {
	return func(i, j int) bool {
		switch {
		case endpointAddresses[i].IP != endpointAddresses[j].IP:
			return endpointAddresses[i].IP < endpointAddresses[j].IP
		case endpointAddresses[i].Hostname != endpointAddresses[j].Hostname:
			return endpointAddresses[i].Hostname < endpointAddresses[j].Hostname
		default:
			return *endpointAddresses[i].NodeName < *endpointAddresses[j].NodeName
		}
	}
}

func (c *HostEndpointsController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	klog.Infof("Starting HostEtcdEndpointsController")
	defer klog.Infof("Shutting down HostEtcdEndpointsController")
	if !cache.WaitForCacheSync(ctx.Done(), c.cachesToSync...) {
		return
	}
	go wait.Until(c.runWorker, time.Second, ctx.Done())
	<-ctx.Done()
}

func (c *HostEndpointsController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *HostEndpointsController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

func (c *HostEndpointsController) eventHandler() cache.ResourceEventHandler {
	// eventHandler queues the operator to check spec and status
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { c.queue.Add(workQueueKey) },
		UpdateFunc: func(old, new interface{}) { c.queue.Add(workQueueKey) },
		DeleteFunc: func(obj interface{}) { c.queue.Add(workQueueKey) },
	}
}
