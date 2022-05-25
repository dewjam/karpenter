/*
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

package provisioning

import (
	"context"
	"fmt"
	"sync"

	"go.uber.org/multierr"

	"github.com/aws/karpenter/pkg/events"

	"github.com/aws/karpenter/pkg/controllers/state"

	"github.com/imdario/mergo"
	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/util/workqueue"
	"knative.dev/pkg/logging"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/controllers/provisioning/scheduling"
	"github.com/aws/karpenter/pkg/metrics"
	"github.com/aws/karpenter/pkg/utils/functional"
	"github.com/aws/karpenter/pkg/utils/injection"
	"github.com/aws/karpenter/pkg/utils/resources"
)

func NewProvisioner(ctx context.Context, kubeClient client.Client, coreV1Client corev1.CoreV1Interface, recorder events.Recorder, cloudProvider cloudprovider.CloudProvider, cluster *state.Cluster) *Provisioner {
	running, stop := context.WithCancel(ctx)
	p := &Provisioner{
		Stop:           stop,
		batcher:        NewBatcher(running),
		cloudProvider:  cloudProvider,
		kubeClient:     kubeClient,
		coreV1Client:   coreV1Client,
		volumeTopology: NewVolumeTopology(kubeClient),
		cluster:        cluster,
		recorder:       recorder,
	}
	p.cond = sync.NewCond(&p.mu)
	go func() {
		for running.Err() == nil {
			if err := p.provision(running); err != nil {
				logging.FromContext(running).Errorf("Provisioning failed, %s", err)
			}
		}
		logging.FromContext(running).Info("Stopped provisioner")
	}()
	return p
}

// Provisioner waits for enqueued pods, batches them, creates capacity and binds the pods to the capacity.
type Provisioner struct {
	// State
	Stop context.CancelFunc
	// Dependencies
	cloudProvider  cloudprovider.CloudProvider
	kubeClient     client.Client
	coreV1Client   corev1.CoreV1Interface
	batcher        *Batcher
	volumeTopology *VolumeTopology
	cluster        *state.Cluster
	recorder       events.Recorder

	mu   sync.Mutex
	cond *sync.Cond
}

func (p *Provisioner) Trigger() {
	p.batcher.Trigger()
}

// Deprecated: TriggerAndWait is used for unit testing purposes only
func (p *Provisioner) TriggerAndWait() {
	p.mu.Lock()
	p.batcher.TriggerImmediate()
	p.cond.Wait()
	p.mu.Unlock()
}

func (p *Provisioner) provision(ctx context.Context) error {
	// Batch pods
	logging.FromContext(ctx).Infof("Waiting for unschedulable pods")
	window := p.batcher.Wait()
	// wake any waiters on the cond
	defer p.cond.Broadcast()

	// Get pods
	pods, err := p.getPods(ctx)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return nil
	}
	logging.FromContext(ctx).Infof("Batched %d pod(s) in %s", len(pods), window)

	// Schedule pods to potential nodes
	nodes, err := p.schedule(ctx, pods)
	if err != nil {
		return err
	}

	// Launch capacity and bind pods
	workqueue.ParallelizeUntil(ctx, len(nodes), len(nodes), func(i int) {
		// create a new context to avoid a data race on the ctx variable
		ctx2 := logging.WithLogger(ctx, logging.FromContext(ctx).With("provisioner", nodes[i].Provisioner.Name))
		// register the provisioner on the context so we can pull it off for tagging purposes
		// TODO: rethink this, maybe just pass the provisioner down instead of hiding it in the context?
		ctx2 = injection.WithNamespacedName(ctx2, client.ObjectKeyFromObject(nodes[i].Provisioner))
		if err := p.launch(ctx2, nodes[i]); err != nil {
			logging.FromContext(ctx2).Errorf("Launching node, %s", err)
		}
	})
	return nil
}

func (p *Provisioner) getPods(ctx context.Context) ([]*v1.Pod, error) {
	var podList v1.PodList
	if err := p.kubeClient.List(ctx, &podList, client.MatchingFields{"spec.nodeName": ""}); err != nil {
		return nil, fmt.Errorf("listing pods, %w", err)
	}
	var pods []*v1.Pod
	for i := range podList.Items {
		pod := podList.Items[i]
		errs := multierr.Combine(
			validate(&pod),
			p.volumeTopology.validatePersistentVolumeClaims(ctx, &pod),
		)
		if errs != nil {
			logging.FromContext(ctx).With("pod", fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)).Debugf("Unable to batch pod, %s", errs)
			continue
		}
		if isProvisionable(&pod) {
			pods = append(pods, &pod)
		}
	}
	return pods, nil
}

func (p *Provisioner) schedule(ctx context.Context, pods []*v1.Pod) ([]*scheduling.Node, error) {
	defer metrics.Measure(schedulingDuration.WithLabelValues(injection.GetNamespacedName(ctx).Name))()
	instanceTypes := make(map[string][]cloudprovider.InstanceType)

	// Build provisioner requirements
	var provisionerList v1alpha5.ProvisionerList
	if err := p.kubeClient.List(ctx, &provisionerList); err != nil {
		return nil, fmt.Errorf("listing provisioners, %w", err)
	}
	var provisioners []*v1alpha5.Provisioner
	for i := range provisionerList.Items {
		provisioner := &provisionerList.Items[i]
		var cloudproviderRequirements v1alpha5.Requirements
		cloudproviderRequirements, err := p.cloudProvider.GetRequirements(ctx, provisioner.Spec.Provider)
		if err != nil {
			return nil, fmt.Errorf("getting provider requirements, %w", err)
		}

		// Get instance type options
		instanceType, err := p.cloudProvider.GetInstanceTypes(ctx, provisioner.Spec.Constraints.Provider)
		if err != nil {
			return nil, fmt.Errorf("getting instance types, %w", err)
		}
		instanceTypes[provisioner.Name] = append(instanceTypes[provisioner.Name], instanceType...)
		instanceTypeRequirements := cloudprovider.Requirements(instanceTypes[provisioner.Name])

		provisioner.Spec.Labels = functional.UnionStringMaps(provisioner.Spec.Labels, map[string]string{v1alpha5.ProvisionerNameLabelKey: provisioner.Name})
		provisioner.Spec.Requirements = v1alpha5.NewRequirements(provisioner.Spec.Requirements.Requirements...).
			Add(v1alpha5.NewLabelRequirements(provisioner.Spec.Labels).Requirements...).
			Add(cloudproviderRequirements.Requirements...).
			Add(instanceTypeRequirements.Requirements...)

		provisioners = append(provisioners, provisioner)
	}

	if len(provisioners) == 0 {
		return nil, fmt.Errorf("no provisioners found")
	}

	// Inject topology requirements
	for _, pod := range pods {
		if err := p.volumeTopology.Inject(ctx, pod); err != nil {
			return nil, fmt.Errorf("getting volume topology requirements, %w", err)
		}
	}

	// Calculate cluster topology
	topology, err := scheduling.NewTopology(ctx, p.kubeClient, p.cluster, provisioners, pods)
	if err != nil {
		return nil, fmt.Errorf("tracking topology counts, %w", err)
	}

	// Calculate daemon overhead
	daemonOverhead, err := p.getDaemonOverhead(ctx, provisioners)
	if err != nil {
		return nil, fmt.Errorf("getting daemon overhead, %w", err)
	}

	return scheduling.NewScheduler(provisioners, p.cluster, topology, instanceTypes, daemonOverhead, p.recorder).Solve(ctx, pods)
}

func (p *Provisioner) launch(ctx context.Context, node *scheduling.Node) error {
	// Check limits
	latest := &v1alpha5.Provisioner{}
	if err := p.kubeClient.Get(ctx, client.ObjectKeyFromObject(node.Provisioner), latest); err != nil {
		return fmt.Errorf("getting current resource usage, %w", err)
	}
	if err := latest.Spec.Limits.ExceededBy(latest.Status.Resources); err != nil {
		return err
	}

	// apply both the taints and startup taints to the node
	taints := append(node.Provisioner.Spec.Taints, node.Provisioner.Spec.StartupTaints...)
	k8sNode, err := p.cloudProvider.Create(ctx, &cloudprovider.NodeRequest{
		InstanceTypeOptions: node.InstanceTypeOptions,
		Template: &cloudprovider.NodeTemplate{
			Provider:             node.Provisioner.Spec.Provider,
			Labels:               node.Provisioner.Spec.Labels,
			Taints:               taints,
			Requirements:         node.Provisioner.Spec.Requirements,
			KubeletConfiguration: node.Provisioner.Spec.KubeletConfiguration,
		},
	})
	if err != nil {
		return fmt.Errorf("creating cloud provider machine, %w", err)
	}

	if err := mergo.Merge(k8sNode, node.Provisioner.Spec.ToNode()); err != nil {
		return fmt.Errorf("merging cloud provider node, %w", err)
	}
	// Idempotently create a node. In rare cases, nodes can come online and
	// self register before the controller is able to register a node object
	// with the API server. In the common case, we create the node object
	// ourselves to enforce the binding decision and enable images to be pulled
	// before the node is fully Ready.
	if _, err := p.coreV1Client.Nodes().Create(ctx, k8sNode, metav1.CreateOptions{}); err != nil {
		if errors.IsAlreadyExists(err) {
			logging.FromContext(ctx).Debugf("node %s already registered", k8sNode.Name)
		} else {
			return fmt.Errorf("creating node %s, %w", k8sNode.Name, err)
		}
	}
	logging.FromContext(ctx).Infof("Created %s", node)
	if err := p.bind(ctx, k8sNode, node.Pods); err != nil {
		return fmt.Errorf("binding pods, %w", err)
	}
	return nil
}

func (p *Provisioner) bind(ctx context.Context, node *v1.Node, pods []*v1.Pod) (err error) {
	defer metrics.Measure(bindTimeHistogram.WithLabelValues(injection.GetNamespacedName(ctx).Name))()

	nodeTaints := v1alpha5.Taints(node.Spec.Taints)

	notReadyTolerations := []v1.Toleration{
		{
			Key:      v1alpha5.NotReadyTaintKey,
			Operator: v1.TolerationOpEqual,
			Effect:   v1.TaintEffectNoSchedule,
		}, {
			Key:      v1.TaintNodeNotReady,
			Operator: v1.TolerationOpEqual,
			Effect:   v1.TaintEffectNoSchedule,
		}}

	workqueue.ParallelizeUntil(ctx, len(pods), len(pods), func(i int) {
		pod := pods[i]
		// Don't bind pods that would immediately get evicted.  We tolerate the two standard taints that are applied for
		// not ready nodes as we are binding pods to these not-ready nodes intentionally (currently).  Binding pods that get
		// evicted can cause extra nodes to be launched as we don't see the in-flight capacity until the pod is fully deleted
		// and controllers sometimes create replacement pods while the existing ones are deleting, but not fully deleted causing
		// us to launch new capacity.
		if nodeTaints.Tolerates(pod, notReadyTolerations...) != nil {
			p.recorder.PodShouldSchedule(pod, node)
			return
		}
		if err := p.coreV1Client.Pods(pods[i].Namespace).Bind(ctx, &v1.Binding{TypeMeta: pod.TypeMeta, ObjectMeta: pod.ObjectMeta, Target: v1.ObjectReference{Name: node.Name}}, metav1.CreateOptions{}); err != nil {
			logging.FromContext(ctx).Errorf("Failed to bind %s/%s to %s, %s", pod.Namespace, pod.Name, node.Name, err)
		}
	})
	return nil
}

func (p *Provisioner) getDaemonOverhead(ctx context.Context, provisioners []*v1alpha5.Provisioner) (map[*v1alpha5.Provisioner]v1.ResourceList, error) {
	overhead := map[*v1alpha5.Provisioner]v1.ResourceList{}

	daemonSetList := &appsv1.DaemonSetList{}
	if err := p.kubeClient.List(ctx, daemonSetList); err != nil {
		return nil, fmt.Errorf("listing daemonsets, %w", err)
	}

	for _, provisioner := range provisioners {
		var daemons []*v1.Pod
		for _, daemonSet := range daemonSetList.Items {
			p := &v1.Pod{Spec: daemonSet.Spec.Template.Spec}
			if err := provisioner.Spec.Taints.Tolerates(p); err != nil {
				continue
			}
			if err := provisioner.Spec.Requirements.Compatible(v1alpha5.NewPodRequirements(p)); err != nil {
				continue
			}
			daemons = append(daemons, p)
		}
		overhead[provisioner] = resources.RequestsForPods(daemons...)
	}

	return overhead, nil
}

var schedulingDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: "allocation_controller",
		Name:      "scheduling_duration_seconds",
		Help:      "Duration of scheduling process in seconds. Broken down by provisioner and error.",
		Buckets:   metrics.DurationBuckets(),
	},
	[]string{metrics.ProvisionerLabel},
)

var bindTimeHistogram = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: metrics.Namespace,
		Subsystem: "allocation_controller",
		Name:      "bind_duration_seconds",
		Help:      "Duration of bind process in seconds. Broken down by result.",
		Buckets:   metrics.DurationBuckets(),
	},
	[]string{metrics.ProvisionerLabel},
)

func init() {
	crmetrics.Registry.MustRegister(bindTimeHistogram)
	crmetrics.Registry.MustRegister(schedulingDuration)
}
