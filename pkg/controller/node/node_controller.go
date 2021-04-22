package node

import (
	"context"
	"fmt"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	nctx "k8s.io/cloud-provider-alibaba-cloud/pkg/context/node"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/context/shared"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/controller/tools"
	"k8s.io/cloud-provider-alibaba-cloud/pkg/provider"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"strings"
	"time"
)

func Add(mgr manager.Manager, ctx *shared.SharedContext) error {
	return add(mgr, newReconciler(mgr, ctx))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, ctx *shared.SharedContext) reconcile.Reconciler {
	recon := &ReconcileNode{
		monitorPeriod:   5 * time.Minute,
		statusFrequency: 5 * time.Minute,
		// provider
		cloud:  ctx.Provider(),
		client: mgr.GetClient(),
		scheme: mgr.GetScheme(),
		record: mgr.GetEventRecorderFor("Node"),
	}
	recon.PeriodicalSync()
	return recon
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New(
		"node-controller", mgr,
		controller.Options{
			Reconciler:              r,
			MaxConcurrentReconciles: 1,
		},
	)
	if err != nil {
		return err
	}

	// Watch for changes to primary resource AutoRepair
	return c.Watch(
		&source.Kind{
			Type: &corev1.Node{},
		},
		&handler.EnqueueRequestForObject{},
	)
}

// ReconcileNode implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileNode{}

// ReconcileNode reconciles a AutoRepair object
type ReconcileNode struct {
	cache  cache.Cache
	cloud  prvd.Provider
	client client.Client
	scheme *runtime.Scheme

	// monitorPeriod controlling monitoring period,
	// i.e. how often does NodeController check node status
	// posted from kubelet. This value should be lower than
	// nodeMonitorGracePeriod set in controller-manager
	monitorPeriod    time.Duration
	statusFrequency  time.Duration
	configCloudRoute bool

	//record event recorder
	record record.EventRecorder
}

func (m *ReconcileNode) Reconcile(
	ctx context.Context, request reconcile.Request,
) (reconcile.Result, error) {
	rlog := log.WithFields(log.Fields{"Node": request.NamespacedName})
	rlog.Infof("watch node change")
	node := &corev1.Node{}
	err := m.client.Get(context.TODO(), request.NamespacedName, node)
	if err != nil {
		if errors.IsNotFound(err) {
			rlog.Infof("node not found, skip")
			// Request object not found, could have been deleted
			// after reconcile request.
			// Owned objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, m.syncCloudNode(node)
}

func (m *ReconcileNode) syncCloudNode(node *corev1.Node) error {
	cloudTaint := findCloudTaint(node.Spec.Taints)
	if cloudTaint == nil {
		log.Infof("node %s is registered without cloud taint. return ok", node.Name)
		return nil
	}
	return m.doAddCloudNode(node)
}

// This processes nodes that were added into the cluster, and cloud initialize them if appropriate
func (m *ReconcileNode) doAddCloudNode(node *corev1.Node) error {
	prvdId := node.Spec.ProviderID
	if prvdId == "" {
		log.Warnf("provider id not exist, skip %s initialize", node.Name)
		return nil
	}

	instance, err := findCloudECS(m.cloud, prvdId)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Warnf("cloud instance not found %s: %v", node.Name, err)
			return nil
		}
		log.Errorf("find ecs: %s", err.Error())
		return fmt.Errorf("find ecs: %s", err.Error())
	}

	// If user provided an IP address, ensure that IP address is found
	// in the cloud provider before removing the taint on the node
	if nodeIP, ok := isProvidedAddrExist(node, instance.Addresses); ok && nodeIP == nil {
		return fmt.Errorf("failed to get specified nodeIP in cloud provider")
	}

	initializer := func() (done bool, err error) {
		log.Infof("try remove cloud taints for %s", node.Name)

		diff := func(copy runtime.Object) (client.Object, error) {
			nins := copy.(*corev1.Node)
			setFields(nins, instance, m.configCloudRoute)
			return nins, nil
		}
		err = tools.PatchM(m.client, node, diff, tools.PatchAll)
		if err != nil {
			log.Errorf("patch node: %s", err.Error())
			return false, nil
		}
		tags := map[string]string{
			"k8s.aliyun.com": "true",
			"kubernetes.ccm": "true",
		}
		err = m.cloud.SetInstanceTags(nctx.NewEmpty(), instance.InstanceID, tags)
		if err != nil {
			if !strings.Contains(err.Error(), "Forbidden.RAM") {
				log.Errorf("tag instance %s error: %s", instance.InstanceID, err.Error())
				//retry
				return false, nil
			}
		}

		log.Infof("finished remove uninitialized cloud taints for %s", node.Name)
		// After adding, call UpdateNodeAddress to set the CloudProvider provided IPAddresses
		// So that users do not see any significant delay in IP addresses being filled into the node
		_ = m.syncNodeAddress([]corev1.Node{*node})
		return true, nil
	}

	err = wait.PollImmediate(2*time.Second, 20*time.Second, initializer)
	if err != nil {
		m.record.Eventf(
			node, corev1.EventTypeWarning, "AddNodeFailed", err.Error(),
		)

		return fmt.Errorf("doAddCloudNode %s error: %s", node.Name, err.Error())
	}

	m.record.Eventf(
		node, corev1.EventTypeNormal, "InitializedNode", "Initialize node successfully",
	)

	log.Infof("Successfully initialized node %s with cloud provider", node.Name)
	return nil
}

// syncNodeAddress updates the nodeAddress
func (m *ReconcileNode) syncNodeAddress(nodes []corev1.Node) error {

	instances, err := m.cloud.ListInstances(nctx.NewEmpty(), nodeids(nodes))
	if err != nil {
		return fmt.Errorf("[NodeAddress] list instances from api: %s", err.Error())
	}

	for i := range nodes {
		node := &nodes[i]
		cloudNode := instances[node.Spec.ProviderID]
		if cloudNode == nil {
			log.Infof("node %s not found, skip update node address", node.Spec.ProviderID)
			continue
		}
		cloudNode.Addresses = setHostnameAddress(node, cloudNode.Addresses)
		// If nodeIP was suggested by user, ensure that
		// it can be found in the cloud as well (consistent with the behaviour in kubelet)
		nodeIP, ok := isProvidedAddrExist(node, cloudNode.Addresses)
		if ok {
			if nodeIP == nil {
				log.Errorf("user specified ip is not found in cloudprovider: %v", node.Status.Addresses)
				continue
			}
			// override addresses
			cloudNode.Addresses = []corev1.NodeAddress{*nodeIP}
		}

		log.Infof("try patch node address for %s", node.Spec.ProviderID)

		diff := func(copy runtime.Object) (client.Object, error) {
			nins := copy.(*corev1.Node)
			nins.Status.Addresses = cloudNode.Addresses
			return nins, nil
		}
		err := tools.PatchM(m.client, node, diff, tools.PatchStatus)
		if err != nil {
			log.Errorf("wait for next retry, patch node address error: %s", err.Error())
			m.record.Eventf(
				node, corev1.EventTypeWarning, "SyncNodeFailed", err.Error(),
			)
		}
	}
	return nil
}

func (m *ReconcileNode) syncNodeExists(nodes []corev1.Node) error {
	instances, err := m.cloud.ListInstances(nctx.NewEmpty(), nodeids(nodes))
	if err != nil {
		return fmt.Errorf("EnsureNodeExists, get instances from api: %s", err.Error())
	}

	for i := range nodes {
		node := &nodes[i]

		condition := nodeConditionReady(m.client, node)
		if condition == nil {
			log.Infof("node %s condition not ready, wait for next retry", node.Spec.ProviderID)
			continue
		}

		if condition.Status == corev1.ConditionTrue {
			// skip ready nodes
			continue
		}

		cloudNode := instances[node.Spec.ProviderID]
		if cloudNode != nil {
			continue
		}

		log.Infof("node %s not found, start to delete from meta", node.Spec.ProviderID)
		// try delete node and ignore error, retry next loop
		deleteNode(m, node)
	}
	return nil
}

func (m *ReconcileNode) PeriodicalSync() {
	// Start a loop to periodically update the node addresses obtained from the cloud
	address := func() {

		nodes, err := NodeList(m.client)
		if err != nil {
			log.Errorf("address sync: %v", err)
			return
		}

		// ignore return value, retry on error
		err = batchOperate(
			nodes.Items,
			m.syncNodeAddress,
		)
		if err != nil {
			log.Errorf("periodically update address: %s", err.Error())
		}
	}
	nodeExists := func() {

		nodes, err := NodeList(m.client)
		if err != nil {
			log.Errorf("node exists sync: %v", err)
			return
		}
		// ignore return value, retry on error
		err = batchOperate(
			nodes.Items,
			m.syncNodeExists,
		)
		if err != nil {
			log.Errorf("periodically try detect node existence: %s", err.Error())
		}
	}

	go wait.Until(address, m.statusFrequency, wait.NeverStop)
	// start a loop to periodically check if any
	// nodes have been deleted from cloudprovider
	go wait.Until(nodeExists, m.monitorPeriod, wait.NeverStop)
}