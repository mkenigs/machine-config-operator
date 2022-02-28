package render

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"path"
	"reflect"
	"time"

	"github.com/golang/glog"
	buildv1 "github.com/openshift/api/build/v1"
	imagev1 "github.com/openshift/api/image/v1"
	buildclientset "github.com/openshift/client-go/build/clientset/versioned"
	imageclientset "github.com/openshift/client-go/image/clientset/versioned"
	mcoResourceApply "github.com/openshift/machine-config-operator/lib/resourceapply"
	mcfgv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"
	ctrlcommon "github.com/openshift/machine-config-operator/pkg/controller/common"
	daemonconsts "github.com/openshift/machine-config-operator/pkg/daemon/constants"
	mcfgclientset "github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned"
	"github.com/openshift/machine-config-operator/pkg/generated/clientset/versioned/scheme"
	mcfginformersv1 "github.com/openshift/machine-config-operator/pkg/generated/informers/externalversions/machineconfiguration.openshift.io/v1"
	mcfglistersv1 "github.com/openshift/machine-config-operator/pkg/generated/listers/machineconfiguration.openshift.io/v1"
	"github.com/openshift/machine-config-operator/pkg/version"
	"github.com/vincent-petithory/dataurl"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	imageinformersv1 "github.com/openshift/client-go/image/informers/externalversions/image/v1"
	imagelistersv1 "github.com/openshift/client-go/image/listers/image/v1"

	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"

	ign3types "github.com/coreos/ignition/v2/config/v3_2/types"
)

const (
	// maxRetries is the number of times a machineconfig pool will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a machineconfig pool is going to be requeued:
	//
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15

	// renderDelay is a pause to avoid churn in MachineConfigs; see
	// https://github.com/openshift/machine-config-operator/issues/301
	renderDelay = 5 * time.Second
)

var (
	// controllerKind contains the schema.GroupVersionKind for this controller type.
	controllerKind = mcfgv1.SchemeGroupVersion.WithKind("MachineConfigPool")

	machineconfigKind = mcfgv1.SchemeGroupVersion.WithKind("MachineConfig")
)

// Controller defines the render controller.
type Controller struct {
	client        mcfgclientset.Interface
	imageclient   imageclientset.Interface
	buildclient   buildclientset.Interface
	kubeclient    clientset.Interface
	eventRecorder record.EventRecorder

	syncHandler              func(mcp string) error
	enqueueMachineConfigPool func(*mcfgv1.MachineConfigPool)

	mcpLister mcfglistersv1.MachineConfigPoolLister
	mcLister  mcfglistersv1.MachineConfigLister

	mcpListerSynced cache.InformerSynced
	mcListerSynced  cache.InformerSynced

	ccLister       mcfglistersv1.ControllerConfigLister
	ccListerSynced cache.InformerSynced

	isLister       imagelistersv1.ImageStreamLister
	isListerSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface
}

// New returns a new render controller.
func New(
	mcpInformer mcfginformersv1.MachineConfigPoolInformer,
	mcInformer mcfginformersv1.MachineConfigInformer,
	ccInformer mcfginformersv1.ControllerConfigInformer,
	isInformer imageinformersv1.ImageStreamInformer,
	kubeClient clientset.Interface,
	mcfgClient mcfgclientset.Interface,
	imageClient imageclientset.Interface,
	buildclient buildclientset.Interface,

) *Controller {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	ctrl := &Controller{
		client:        mcfgClient,
		kubeclient:    kubeClient,
		imageclient:   imageClient,
		buildclient:   buildclient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "machineconfigcontroller-rendercontroller"}),
		queue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "machineconfigcontroller-rendercontroller"),
	}

	mcpInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addMachineConfigPool,
		UpdateFunc: ctrl.updateMachineConfigPool,
		DeleteFunc: ctrl.deleteMachineConfigPool,
	})
	mcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addMachineConfig,
		UpdateFunc: ctrl.updateMachineConfig,
		DeleteFunc: ctrl.deleteMachineConfig,
	})

	isInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    ctrl.addImageStream,
		UpdateFunc: ctrl.updateImageStream,
		DeleteFunc: ctrl.deleteImageStream,
	})

	ctrl.syncHandler = ctrl.syncMachineConfigPool
	ctrl.enqueueMachineConfigPool = ctrl.enqueueDefault

	ctrl.mcpLister = mcpInformer.Lister()
	ctrl.mcLister = mcInformer.Lister()
	ctrl.mcpListerSynced = mcpInformer.Informer().HasSynced
	ctrl.mcListerSynced = mcInformer.Informer().HasSynced
	ctrl.ccLister = ccInformer.Lister()
	ctrl.ccListerSynced = ccInformer.Informer().HasSynced

	ctrl.isLister = isInformer.Lister()
	ctrl.isListerSynced = isInformer.Informer().HasSynced

	return ctrl
}

// Run executes the render controller.
func (ctrl *Controller) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer ctrl.queue.ShutDown()

	if !cache.WaitForCacheSync(stopCh, ctrl.mcpListerSynced, ctrl.mcListerSynced, ctrl.ccListerSynced, ctrl.isListerSynced) {
		return
	}

	glog.Info("Starting MachineConfigController-RenderController")
	defer glog.Info("Shutting down MachineConfigController-RenderController")

	for i := 0; i < workers; i++ {
		go wait.Until(ctrl.worker, time.Second, stopCh)
	}

	<-stopCh
}

func (ctrl *Controller) addMachineConfigPool(obj interface{}) {
	pool := obj.(*mcfgv1.MachineConfigPool)
	glog.V(4).Infof("Adding MachineConfigPool %s", pool.Name)
	ctrl.enqueueMachineConfigPool(pool)

}

func (ctrl *Controller) updateMachineConfigPool(old, cur interface{}) {
	oldPool := old.(*mcfgv1.MachineConfigPool)
	curPool := cur.(*mcfgv1.MachineConfigPool)

	glog.V(4).Infof("Updating MachineConfigPool %s", oldPool.Name)
	ctrl.enqueueMachineConfigPool(curPool)
}

func (ctrl *Controller) deleteMachineConfigPool(obj interface{}) {
	pool, ok := obj.(*mcfgv1.MachineConfigPool)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		pool, ok = tombstone.Obj.(*mcfgv1.MachineConfigPool)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a MachineConfigPool %#v", obj))
			return
		}
	}
	glog.V(4).Infof("Deleting MachineConfigPool %s", pool.Name)
	// TODO(abhinavdahiya): handle deletes.
}

func (ctrl *Controller) addMachineConfig(obj interface{}) {
	mc := obj.(*mcfgv1.MachineConfig)
	if mc.DeletionTimestamp != nil {
		ctrl.deleteMachineConfig(mc)
		return
	}

	controllerRef := metav1.GetControllerOf(mc)
	if controllerRef != nil {
		if pool := ctrl.resolveControllerRef(controllerRef); pool != nil {
			glog.V(4).Infof("MachineConfig %s added", mc.Name)
			ctrl.enqueueMachineConfigPool(pool)
			return
		}
	}

	pools, err := ctrl.getPoolsForMachineConfig(mc)
	if err != nil {
		glog.Errorf("error finding pools for machineconfig: %v", err)
		return
	}

	glog.V(4).Infof("MachineConfig %s added", mc.Name)
	for _, p := range pools {
		ctrl.enqueueMachineConfigPool(p)
	}
}

func (ctrl *Controller) updateMachineConfig(old, cur interface{}) {
	oldMC := old.(*mcfgv1.MachineConfig)
	curMC := cur.(*mcfgv1.MachineConfig)

	curControllerRef := metav1.GetControllerOf(curMC)
	oldControllerRef := metav1.GetControllerOf(oldMC)
	controllerRefChanged := !reflect.DeepEqual(curControllerRef, oldControllerRef)
	if controllerRefChanged && oldControllerRef != nil {
		glog.Errorf("machineconfig has changed controller, not allowed.")
		return
	}

	if curControllerRef != nil {
		if pool := ctrl.resolveControllerRef(curControllerRef); pool != nil {
			glog.V(4).Infof("MachineConfig %s updated", curMC.Name)
			ctrl.enqueueMachineConfigPool(pool)
			return
		}
	}

	pools, err := ctrl.getPoolsForMachineConfig(curMC)
	if err != nil {
		glog.Errorf("error finding pools for machineconfig: %v", err)
		return
	}

	glog.V(4).Infof("MachineConfig %s updated", curMC.Name)
	for _, p := range pools {
		ctrl.enqueueMachineConfigPool(p)
	}
}

func (ctrl *Controller) deleteMachineConfig(obj interface{}) {
	mc, ok := obj.(*mcfgv1.MachineConfig)

	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		mc, ok = tombstone.Obj.(*mcfgv1.MachineConfig)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("Tombstone contained object that is not a MachineConfig %#v", obj))
			return
		}
	}

	controllerRef := metav1.GetControllerOf(mc)
	if controllerRef != nil {
		if pool := ctrl.resolveControllerRef(controllerRef); pool != nil {
			glog.V(4).Infof("MachineConfig %s deleted", mc.Name)
			ctrl.enqueueMachineConfigPool(pool)
			return
		}
	}

	pools, err := ctrl.getPoolsForMachineConfig(mc)
	if err != nil {
		glog.Errorf("error finding pools for machineconfig: %v", err)
		return
	}

	glog.V(4).Infof("MachineConfig %s deleted", mc.Name)
	for _, p := range pools {
		ctrl.enqueueMachineConfigPool(p)
	}
}

func (ctrl *Controller) resolveControllerRef(controllerRef *metav1.OwnerReference) *mcfgv1.MachineConfigPool {
	// We can't look up by UID, so look up by Name and then verify UID.
	// Don't even try to look up by Name if it's the wrong Kind.
	if controllerRef.Kind != controllerKind.Kind {
		return nil
	}
	pool, err := ctrl.mcpLister.Get(controllerRef.Name)
	if err != nil {
		return nil
	}

	if pool.UID != controllerRef.UID {
		// The controller we found with this Name is not the same one that the
		// ControllerRef points to.
		return nil
	}
	return pool
}

func (ctrl *Controller) getPoolsForMachineConfig(config *mcfgv1.MachineConfig) ([]*mcfgv1.MachineConfigPool, error) {
	if len(config.Labels) == 0 {
		return nil, fmt.Errorf("no MachineConfigPool found for MachineConfig %v because it has no labels", config.Name)
	}

	pList, err := ctrl.mcpLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var pools []*mcfgv1.MachineConfigPool
	for _, p := range pList {
		selector, err := metav1.LabelSelectorAsSelector(p.Spec.MachineConfigSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid label selector: %v", err)
		}

		// If a pool with a nil or empty selector creeps in, it should match nothing, not everything.
		if selector.Empty() || !selector.Matches(labels.Set(config.Labels)) {
			continue
		}

		pools = append(pools, p)
	}

	if len(pools) == 0 {
		return nil, fmt.Errorf("could not find any MachineConfigPool set for MachineConfig %s with labels: %v", config.Name, config.Labels)
	}
	return pools, nil
}

func (ctrl *Controller) enqueue(pool *mcfgv1.MachineConfigPool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", pool, err))
		return
	}

	ctrl.queue.Add(key)
}

func (ctrl *Controller) enqueueRateLimited(pool *mcfgv1.MachineConfigPool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", pool, err))
		return
	}

	ctrl.queue.AddRateLimited(key)
}

// enqueueAfter will enqueue a pool after the provided amount of time.
func (ctrl *Controller) enqueueAfter(pool *mcfgv1.MachineConfigPool, after time.Duration) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(pool)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Couldn't get key for object %#v: %v", pool, err))
		return
	}

	ctrl.queue.AddAfter(key, after)
}

// enqueueDefault calls a default enqueue function
func (ctrl *Controller) enqueueDefault(pool *mcfgv1.MachineConfigPool) {
	ctrl.enqueueAfter(pool, renderDelay)
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (ctrl *Controller) worker() {
	for ctrl.processNextWorkItem() {
	}
}

func (ctrl *Controller) processNextWorkItem() bool {
	key, quit := ctrl.queue.Get()
	if quit {
		return false
	}
	defer ctrl.queue.Done(key)

	err := ctrl.syncHandler(key.(string))
	ctrl.handleErr(err, key)

	return true
}

func (ctrl *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		ctrl.queue.Forget(key)
		return
	}

	if ctrl.queue.NumRequeues(key) < maxRetries {
		glog.V(2).Infof("Error syncing machineconfigpool %v: %v", key, err)
		ctrl.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	glog.V(2).Infof("Dropping machineconfigpool %q out of the queue: %v", key, err)
	ctrl.queue.Forget(key)
	ctrl.queue.AddAfter(key, 1*time.Minute)
}

// syncMachineConfigPool will sync the machineconfig pool with the given key.
// This function is not meant to be invoked concurrently with the same key.
func (ctrl *Controller) syncMachineConfigPool(key string) error {
	startTime := time.Now()
	glog.V(4).Infof("Started syncing machineconfigpool %q (%v)", key, startTime)
	defer func() {
		glog.V(4).Infof("Finished syncing machineconfigpool %q (%v)", key, time.Since(startTime))
	}()

	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	machineconfigpool, err := ctrl.mcpLister.Get(name)
	if errors.IsNotFound(err) {
		glog.V(2).Infof("MachineConfigPool %v has been deleted", key)
		return nil
	}
	if err != nil {
		return err
	}

	// Deep-copy otherwise we are mutating our cache.
	// TODO: Deep-copy only when needed.
	pool := machineconfigpool.DeepCopy()
	everything := metav1.LabelSelector{}
	if reflect.DeepEqual(pool.Spec.MachineConfigSelector, &everything) {
		ctrl.eventRecorder.Eventf(pool, corev1.EventTypeWarning, "SelectingAll", "This machineconfigpool is selecting all machineconfigs. A non-empty selector is required.")
		return nil
	}

	// If this is our special pool, check to see if we have an image stream
	// If we do, snarf out the latest image and annotate the pool with it
	// Trying "pull" based on an enqueue from the image stream informer rather than push
	if pool.Name == "layered" {
		// Do we have an image stream
		is, err := ctrl.imageclient.ImageV1().ImageStreams("openshift-machine-config-operator").Get(context.TODO(), "mco-content-"+pool.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			glog.Warningf("Image stream for %s does not exist (yet?): %s", pool.Name, err)
		} else {

			// If we do, grab the latest tag
			for _, tag := range is.Status.Tags {
				for _, image := range tag.Items {

					// If this is different than our current tag, grab it and annotate the pool
					if imageReference, ok := pool.Annotations[ctrlcommon.ExperimentalNewestLayeredImageAnnotationKey]; !ok || imageReference != image.DockerImageReference {
						glog.Infof("imagestream %s newest is: %s", is.Name, image.DockerImageReference)
						// Annotate the pool with its most recent image so node-controller can use it
						// TODO(jkyros): should be node controller eventually that would "assign" a node for upgrade, but for now we're confined to a single pool
						pool.Annotations[ctrlcommon.ExperimentalNewestLayeredImageAnnotationKey] = image.DockerImageReference

						// get the actual image so we can read its labels
						fullImage, err := ctrl.imageclient.ImageV1().Images().Get(context.TODO(), image.Image, metav1.GetOptions{})
						if err != nil {
							return err
						}

						// We need the labels out of the docker image but it's a raw extension
						dockerLabels := struct {
							Config struct {
								Labels map[string]string `json:"Labels"`
							} `json:"Config"`
						}{}

						// Get the labels out and see what config this is
						err = json.Unmarshal(fullImage.DockerImageMetadata.Raw, &dockerLabels)
						if err != nil {
							glog.Warningf("Could not get labels from docker image metadata: %s", err)
						}

						// Tag what config this came from so we know it's the right image
						if machineconfig, ok := dockerLabels.Config.Labels["machineconfig"]; ok {
							pool.Annotations[ctrlcommon.ExperimentalNewestLayeredImageEquivalentConfigAnnotationKey] = machineconfig
						}

						ctrl.eventRecorder.Event(pool, corev1.EventTypeNormal, "Updated", "Moved pool "+pool.Name+" to layered image "+image.DockerImageReference)

					}
					// TODO(jkyros): right now I only want the first one, this should probably just be be is.Status.Tags[0] instead of the loop
					break
				}
			}
		}
	}

	selector, err := metav1.LabelSelectorAsSelector(pool.Spec.MachineConfigSelector)
	if err != nil {
		return err
	}

	// TODO(runcom): add tests in render_controller_test.go for this condition
	if err := mcfgv1.IsControllerConfigCompleted(ctrlcommon.ControllerConfigName, ctrl.ccLister.Get); err != nil {
		return err
	}

	mcs, err := ctrl.mcLister.List(selector)
	if err != nil {
		return err
	}
	if len(mcs) == 0 {
		return ctrl.syncFailingStatus(pool, fmt.Errorf("no MachineConfigs found matching selector %v", selector))
	}

	if err := ctrl.syncGeneratedMachineConfig(pool, mcs); err != nil {
		return ctrl.syncFailingStatus(pool, err)
	}

	return ctrl.syncAvailableStatus(pool)
}

func (ctrl *Controller) syncAvailableStatus(pool *mcfgv1.MachineConfigPool) error {
	if mcfgv1.IsMachineConfigPoolConditionFalse(pool.Status.Conditions, mcfgv1.MachineConfigPoolRenderDegraded) {
		return nil
	}
	sdegraded := mcfgv1.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolRenderDegraded, corev1.ConditionFalse, "", "")
	mcfgv1.SetMachineConfigPoolCondition(&pool.Status, *sdegraded)
	if _, err := ctrl.client.MachineconfigurationV1().MachineConfigPools().UpdateStatus(context.TODO(), pool, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func (ctrl *Controller) syncFailingStatus(pool *mcfgv1.MachineConfigPool, err error) error {
	sdegraded := mcfgv1.NewMachineConfigPoolCondition(mcfgv1.MachineConfigPoolRenderDegraded, corev1.ConditionTrue, "", fmt.Sprintf("Failed to render configuration for pool %s: %v", pool.Name, err))
	mcfgv1.SetMachineConfigPoolCondition(&pool.Status, *sdegraded)
	if _, updateErr := ctrl.client.MachineconfigurationV1().MachineConfigPools().UpdateStatus(context.TODO(), pool, metav1.UpdateOptions{}); updateErr != nil {
		glog.Errorf("Error updating MachineConfigPool %s: %v", pool.Name, updateErr)
	}
	return err
}

// This function will eventually contain a sane garbage collection policy for rendered MachineConfigs;
// see https://github.com/openshift/machine-config-operator/issues/301
// It will probably involve making sure we're only GCing a config after all nodes don't have it
// in either desired or current config.
func (ctrl *Controller) garbageCollectRenderedConfigs(pool *mcfgv1.MachineConfigPool) error {
	// Temporarily until https://github.com/openshift/machine-config-operator/pull/318
	// which depends on the strategy for https://github.com/openshift/machine-config-operator/issues/301
	return nil
}

func (ctrl *Controller) syncGeneratedMachineConfig(pool *mcfgv1.MachineConfigPool, configs []*mcfgv1.MachineConfig) error {
	if len(configs) == 0 {
		return nil
	}

	cc, err := ctrl.ccLister.Get(ctrlcommon.ControllerConfigName)
	if err != nil {
		return err
	}

	generated, err := generateRenderedMachineConfig(pool, configs, cc)
	if err != nil {
		return err
	}

	source := []corev1.ObjectReference{}
	for _, cfg := range configs {
		source = append(source, corev1.ObjectReference{Kind: machineconfigKind.Kind, Name: cfg.GetName(), APIVersion: machineconfigKind.GroupVersion().String()})
	}

	_, err = ctrl.mcLister.Get(generated.Name)
	if apierrors.IsNotFound(err) {
		_, err = ctrl.client.MachineconfigurationV1().MachineConfigs().Create(context.TODO(), generated, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		glog.V(2).Infof("Generated machineconfig %s from %d configs: %s", generated.Name, len(source), source)
		ctrl.eventRecorder.Eventf(pool, corev1.EventTypeNormal, "RenderedConfigGenerated", "%s successfully generated", generated.Name)
	}
	if err != nil {
		return err
	}

	newPool := pool.DeepCopy()
	newPool.Spec.Configuration.Source = source

	if pool.Spec.Configuration.Name == generated.Name {
		_, _, err = mcoResourceApply.ApplyMachineConfig(ctrl.client.MachineconfigurationV1(), generated)
		if err != nil {
			return err
		}
		_, err = ctrl.client.MachineconfigurationV1().MachineConfigPools().Update(context.TODO(), newPool, metav1.UpdateOptions{})
		return err
	}

	newPool.Spec.Configuration.Name = generated.Name
	// TODO(walters) Use subresource or JSON patch, but the latter isn't supported by the unit test mocks
	pool, err = ctrl.client.MachineconfigurationV1().MachineConfigPools().Update(context.TODO(), newPool, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	glog.V(2).Infof("Pool %s: now targeting: %s", pool.Name, pool.Spec.Configuration.Name)

	// TODO(jkyros): Hacked this in, take it out later when/if you figure out where it should go
	// I initially picked here rather than in node-controller because I wasn't sure if we'd need to update OsImageURL or
	// include special files in our render (e.g. machineconfig-to-ignition, etc)
	if pool.Name == "layered" {

		glog.Infof("Pool %s is s special pool, rendering config to imagestream", pool.Name)
		// jkyros: Just put this here for now since it's convenient
		err = ctrl.experimentalRenderToImagestream(pool, generated)
		if err != nil {
			glog.Warningf("Failed to EXPERIMENTALLY render to image stream: %s", err)
		} else {

		}
	}

	if err := ctrl.garbageCollectRenderedConfigs(pool); err != nil {
		return err
	}

	return nil
}

// generateRenderedMachineConfig takes all MCs for a given pool and returns a single rendered MC. For ex master-XXXX or worker-XXXX
func generateRenderedMachineConfig(pool *mcfgv1.MachineConfigPool, configs []*mcfgv1.MachineConfig, cconfig *mcfgv1.ControllerConfig) (*mcfgv1.MachineConfig, error) {
	// Suppress rendered config generation until a corresponding new controller can roll out too.
	// https://bugzilla.redhat.com/show_bug.cgi?id=1879099
	if genver, ok := cconfig.Annotations[daemonconsts.GeneratedByVersionAnnotationKey]; ok {
		if genver != version.Raw {
			return nil, fmt.Errorf("Ignoring controller config generated from %s (my version: %s)", genver, version.Raw)
		}
	} else {
		return nil, fmt.Errorf("Ignoring controller config generated without %s annotation (my version: %s)", daemonconsts.GeneratedByVersionAnnotationKey, version.Raw)
	}

	// Before merging all MCs for a specific pool, let's make sure MachineConfigs are valid
	for _, config := range configs {
		if err := ctrlcommon.ValidateMachineConfig(config.Spec); err != nil {
			return nil, err
		}
	}

	merged, err := ctrlcommon.MergeMachineConfigs(configs, cconfig.Spec.OSImageURL)
	if err != nil {
		return nil, err
	}
	hashedName, err := getMachineConfigHashedName(pool, merged)
	if err != nil {
		return nil, err
	}
	oref := metav1.NewControllerRef(pool, controllerKind)

	merged.SetName(hashedName)
	merged.SetOwnerReferences([]metav1.OwnerReference{*oref})
	if merged.Annotations == nil {
		merged.Annotations = map[string]string{}
	}
	merged.Annotations[ctrlcommon.GeneratedByControllerVersionAnnotationKey] = version.Hash

	return merged, nil
}

// RunBootstrap runs the render controller in bootstrap mode.
// For each pool, it matches the machineconfigs based on label selector and
// returns the generated machineconfigs and pool with CurrentMachineConfig status field set.
func RunBootstrap(pools []*mcfgv1.MachineConfigPool, configs []*mcfgv1.MachineConfig, cconfig *mcfgv1.ControllerConfig) ([]*mcfgv1.MachineConfigPool, []*mcfgv1.MachineConfig, error) {
	var (
		opools   []*mcfgv1.MachineConfigPool
		oconfigs []*mcfgv1.MachineConfig
	)
	for _, pool := range pools {
		pcs, err := getMachineConfigsForPool(pool, configs)
		if err != nil {
			return nil, nil, err
		}

		generated, err := generateRenderedMachineConfig(pool, pcs, cconfig)
		if err != nil {
			return nil, nil, err
		}

		source := []corev1.ObjectReference{}
		for _, cfg := range configs {
			source = append(source, corev1.ObjectReference{Kind: machineconfigKind.Kind, Name: cfg.GetName(), APIVersion: machineconfigKind.GroupVersion().String()})
		}

		pool.Spec.Configuration.Name = generated.Name
		pool.Spec.Configuration.Source = source
		pool.Status.Configuration.Name = generated.Name
		pool.Status.Configuration.Source = source
		opools = append(opools, pool)
		oconfigs = append(oconfigs, generated)
	}
	return opools, oconfigs, nil
}

// getMachineConfigsForPool is called by RunBootstrap and returns configs that match label from configs for a pool.
func getMachineConfigsForPool(pool *mcfgv1.MachineConfigPool, configs []*mcfgv1.MachineConfig) ([]*mcfgv1.MachineConfig, error) {
	selector, err := metav1.LabelSelectorAsSelector(pool.Spec.MachineConfigSelector)
	if err != nil {
		return nil, fmt.Errorf("invalid label selector: %v", err)
	}

	var out []*mcfgv1.MachineConfig

	// If a pool with a nil or empty selector creeps in, it should match nothing
	if selector.Empty() {
		return out, nil
	}
	for idx, config := range configs {
		if selector.Matches(labels.Set(config.Labels)) {
			out = append(out, configs[idx])
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("couldn't find any MachineConfigs for pool: %v", pool.Name)
	}
	return out, nil
}

// experimentalAddBuildConfigs creates a build config for a pool so that its content automatically
// gets rendered into an image when the machineconfig content changes
func (ctrl *Controller) experimentalAddBuildConfigs(pool *mcfgv1.MachineConfigPool) error {

	buildConfigName := fmt.Sprintf("mco-build-content-%s", pool.Name)
	contentImageStreamName := fmt.Sprintf("mco-content-%s", pool.Name)
	var targetNamespace = "openshift-machine-config-operator"

	// Multistage build, butane is optional (the [l] character class makes it a 'wildcard' and it won't fail if it doesn't exist)
	var dockerFile = `
	FROM image-registry.openshift-image-registry.svc:5000/openshift-machine-config-operator/rendered-layered AS machineconfig
	FROM image-registry.openshift-image-registry.svc:5000/openshift-machine-config-operator/coreos
	#You'd think this would work, but it doesn't in a buildconfig
	#COPY --from=machineconfig /mco-butane.yaml* /etc/
	COPY --from=machineconfig /machineconfig.json /etc/mco-content-machineconfig.json
	COPY --from=machineconfig /ignition.json /etc/mco-content-ignition.json
	ENV container=1
	RUN ignition-liveapply /etc/mco-content-ignition.json
	#This tells me there are no configured repos? hmmmm
	#RUN rpm-ostree ex rebuild && rm -rf /var/cache /etc/rpm-ostree/origin.d
	ARG machineconfig=unknown
	#Injected from mcc's buildrequest so we can use it later
	LABEL machineconfig=$machineconfig
	`

	// TODO(jkyros): ownerships!
	_, err := ctrl.buildclient.BuildV1().BuildConfigs(targetNamespace).Get(context.TODO(), buildConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {

		// Construct a buildconfig for this pool if it doesn't exist
		buildConfig := &buildv1.BuildConfig{

			TypeMeta: metav1.TypeMeta{},
			ObjectMeta: metav1.ObjectMeta{
				Name:      buildConfigName,
				Namespace: targetNamespace,
				Annotations: map[string]string{
					"machineconfiguration.openshift.io/pool": pool.Name,
				},
			},
			Spec: buildv1.BuildConfigSpec{
				// Trigger when our "machineconfig" image gets updated on a render

				// NOTE(jkyros): I took this out because we're triggering manually now
				// I had to so that we could get the metadata in there. If we can find another way
				// to capture the metadata, the imagestream could do the triggering
				/*	Triggers: []buildv1.BuildTriggerPolicy{
					{
						ImageChange: &buildv1.ImageChangeTrigger{
							From: &corev1.ObjectReference{
								Kind: "ImageStreamTag",
								Name: "rendered-" + pool.Name + ":latest",
							},
							Paused: false,
						},
						Type: buildv1.ImageChangeBuildTriggerType,
					},
				}, */
				RunPolicy: "Serial",
				// Simple dockerfile build, just the text from above
				CommonSpec: buildv1.CommonSpec{
					Source: buildv1.BuildSource{
						Type:       "Dockerfile",
						Dockerfile: &dockerFile,
					},
					Strategy: buildv1.BuildStrategy{
						DockerStrategy: &buildv1.DockerBuildStrategy{},
						Type:           "Docker",
					},
					// Output to the imagestreams we made before
					Output: buildv1.BuildOutput{
						To: &corev1.ObjectReference{
							Kind: "ImageStreamTag",
							Name: contentImageStreamName + ":latest",
						},
						//TODO(jkyros): I want to label these images with which rendered config they were built from
						// but there doesn't seem to be a way to get it in there easily
					},
				},
			},
		}

		//TODO(jkyros): needs to be a better way to stuff it in that struct up there, but this works
		poolKind := mcfgv1.SchemeGroupVersion.WithKind("MachineConfigPool")
		oref := metav1.NewControllerRef(pool, poolKind)
		buildConfig.SetOwnerReferences([]metav1.OwnerReference{*oref})

		// Construct the buildconfig for this pool that will build an image out of the MCO content

		// Create the buildconfig
		_, err := ctrl.buildclient.BuildV1().BuildConfigs(targetNamespace).Create(context.TODO(), buildConfig, metav1.CreateOptions{})
		if err != nil {
			return err
		}

		glog.Infof("Buildconfig %s has been created for pool %s", buildConfigName, pool.Name)
	} else if err != nil {
		// some other error happened
		return err
	} else {
		// already existed, do nothing
		glog.Infof("Buildconfig %s already existed for pool %s", buildConfigName, pool.Name)
	}

	return nil
}

// experimentalRenderToImagestream is a dirty hack that sets up imagestream rendering of config (hopefully) without interrupting normal operation :)
func (ctrl *Controller) experimentalRenderToImagestream(pool *mcfgv1.MachineConfigPool, generated *mcfgv1.MachineConfig) error {

	var err error
	var targetNamespace = "openshift-machine-config-operator"
	// TODO(jkyros): look up the namespace

	// Make sure we have an image stream to render content into
	renderedImageStreamName := fmt.Sprintf("rendered-%s", pool.Name)
	// And also, an imagestream to receive our "coreos-derive" build image
	contentImageStreamName := fmt.Sprintf("mco-content-%s", pool.Name)

	// Our list of imagestreams we need to ensure exists
	var ensureImageStreams = []string{renderedImageStreamName, contentImageStreamName, "coreos"}

	glog.V(2).Infof("Ensuring image streams exist for pool %s", pool.Name)
	// TODO(jkyros): This should probably be a controller thing somewhere
	for _, imageStreamName := range ensureImageStreams {

		_, err = ctrl.imageclient.ImageV1().ImageStreams(targetNamespace).Get(context.TODO(), imageStreamName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			// If we don't, we need to make it, otherwise our builds will fail

			createMe := &imagev1.ImageStream{}
			createMe.Name = imageStreamName
			createMe.Namespace = targetNamespace
			createMe.Spec.LookupPolicy.Local = false

			// Set ownerships so these get cleaned up if we delete the pool
			// TODO(jkyros): I have no idea if this actually cleans the images out of the stream if we delete it?
			poolKind := mcfgv1.SchemeGroupVersion.WithKind("MachineConfigPool")
			oref := metav1.NewControllerRef(pool, poolKind)
			createMe.SetOwnerReferences([]metav1.OwnerReference{*oref})

			// coreos imagestream is base, it's special, it needs to pull that image
			if imageStreamName == "coreos" {
				createMe.Spec = imagev1.ImageStreamSpec{
					LookupPolicy:          imagev1.ImageLookupPolicy{Local: false},
					DockerImageRepository: "",
					Tags: []imagev1.TagReference{
						{
							Name: "latest",
							From: &corev1.ObjectReference{
								Kind: "DockerImage",
								Name: "quay.io/jkyros/derived-images:rhcos-new-format-with-new-binaries",
							},
						},
					},
				}

			}

			// It didn't exist, put the imagestream in the cluster
			_, err := ctrl.imageclient.ImageV1().ImageStreams(targetNamespace).Create(context.TODO(), createMe, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("Attempted to create imagestream %s but failed: ", err)
			}
			glog.Infof("Created image stream %s", imageStreamName)
		} else if err != nil {
			// I don't know if it existed or not, I couldn't get it
			return fmt.Errorf("Failed to retrieve imagestream %s: %s", imageStreamName, err)
		}

		glog.Infof("Image stream %s already exists", renderedImageStreamName)
	}

	// Now that we have our imagestreams, set up the buildconfigs
	err = ctrl.experimentalAddBuildConfigs(pool)
	if err != nil {
		return err
	}

	glog.V(2).Infof("Triggering image build for machineconfig %s", generated.Name)

	// NOTE: below here essentially packs a tiny image containing our machine config content
	// and stuffs it into the integrated repository using our servicetoken. I used "crane" because
	// it was simple and straightforward, there's probably a podman/buildah/libcontainer equivalent
	// that I should use instead.

	// Get the ignition out of the MachineConfig
	ignConf, err := ctrlcommon.ParseAndConvertConfig(generated.Spec.Config.Raw)
	if err != nil {
		glog.Errorf("parsing Ignition config failed with error: %v", err)
	}

	// Convert it to json
	ignJSON, err := json.MarshalIndent(ignConf, "  ", "    ")
	if err != nil {
		glog.Warningf("Failed to marshal config for container: %s", err)
	}

	// Convert the MachineConfig to JSON also just in case we want it
	machineConfigJSON, err := json.MarshalIndent(generated, "  ", "    ")
	if err != nil {
		glog.Warningf("Failed to marshal config for container: %s", err)
	}

	//TODO(jkyros): we could potentially expose the component machineconfigs or metadata here too

	// This is the "file guts" for our "layer "
	var fileMap = map[string][]byte{
		"/machineconfig.json": machineConfigJSON,
		"/ignition.json":      ignJSON,
	}

	// Lol I know, let's make this more compicated with nesting
	// Add the butane to the image if we have some in our machineconfig
	extractedButane, err := ctrlcommon.GetIgnitionFileDataByPath(&ignConf, "/etc/mco-butane.yaml")
	if err == nil && extractedButane != nil {
		fileMap["/butane.yaml"] = extractedButane
	}

	// This is essentially "FROM scratch"
	// ADD /machineconfig.json
	// ADD /ignition.json
	newImg, err := crane.Image(fileMap)
	if err != nil {
		glog.Warningf("Failed to create image layer: %s", err)
	}

	// Stuff a label in to see what happens when we build
	newImg, err = mutate.Config(newImg, v1.Config{
		Labels: map[string]string{"jkyros": "testing", "machineconfig": generated.Name},
	})

	// Tag it so we can push it into the integrated registry
	// I added a RoleBinding manifest that gives us permission to push to the repo with our service account
	contentTag := path.Join("image-registry.openshift-image-registry.svc:5000", targetNamespace, renderedImageStreamName)
	tag, err := name.NewTag(contentTag + ":latest")
	if err != nil {
		return fmt.Errorf("Failed to create new tag %s: %s", contentTag, err)
	}

	// TODO(jkyros): handle the certificates properly rather than turning checking off
	tr := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	crane.WithTransport(tr)

	// Retrieve our service account token (I know there is a more kubernetes-y way to do this, but this works for now)
	stoken, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return fmt.Errorf("Unable to get service token: %s", stoken)
	}

	// Create our auth object to auth to the integrated repo
	au := &authn.Basic{
		Username: "machine-config-controller",
		Password: string(stoken),
	}

	// Push our image into the integrated repo
	if err := crane.Push(newImg, tag.String(), crane.WithTransport(tr), crane.WithAuth(au)); err != nil {
		return fmt.Errorf("Failed to push image %s: %s", tag.String(), err)
	} else {
		glog.Infof("Image %s successfully pushed.", tag.String())
	}

	// TODO(jkyros): I wonder if I can use an EnvVarSource here and reference something without having to stuff this in
	var whichConfig = corev1.EnvVar{
		Name:  "machineconfig",
		Value: generated.Name,
	}

	// create a buildconfigrequest to trigger the build because if we let the imagestream do it on its own,
	// it won't have any of the metadata, and we can't be sure which image is which
	// It looks like the imagetag object that comes out will let me see the labels and the env, so I could just stuff it in env but that seems hacky
	// TODO(jkyros): If you skip the buildconfig and just make this a build, then I think you can just supply the labels,
	// but then nobody can see the buildconfig?
	br := &buildv1.BuildRequest{
		//TypeMeta:    metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("mco-build-content-%s", pool.Name)},
		//Revision:    &buildv1.SourceRevision{},
		//Env: []corev1.EnvVar{whichConfig},
		TriggeredBy: []buildv1.BuildTriggerCause{
			{Message: "The machine config controller"},
		},
		DockerStrategyOptions: &buildv1.DockerStrategyOptions{
			BuildArgs: []corev1.EnvVar{whichConfig},
			//NoCache:   new(bool),
		},
	}

	// Trigger our build manually, we don't have to wait, the pool will figure it out when it's available
	_, err = ctrl.buildclient.BuildV1().BuildConfigs(targetNamespace).Instantiate(context.TODO(), br.Name, br, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("Failed to trigger image build build: %s", err)
	}

	// After the image gets pushed, because of the image triggers, the buildconfigs will do their builds automatically
	// And the mco-content-%pool imagestream will contain an image with the MCO content from applied to it

	return nil
}

// TODO(jkyros): don't leave this here, expose it properly if you're gonna use it
// StrToPtr returns a pointer to a string
func StrToPtr(s string) *string {
	return &s
}

// TODO(jkyros): don't leave this here, expose it properly if you're gonna use it
// NewIgnFile returns a simple ignition3 file from just path and file contents
func NewIgnFile(path, contents string) ign3types.File {
	return ign3types.File{
		Node: ign3types.Node{
			Path: path,
		},
		FileEmbedded1: ign3types.FileEmbedded1{
			Contents: ign3types.Resource{
				Source: StrToPtr(dataurl.EncodeBytes([]byte(contents)))},
		},
	}
}

// TODO(jkyros): some quick functions to go with our image stream informer so we can watch imagestream update
func (ctrl *Controller) addImageStream(obj interface{}) {
	imagestream := obj.(*imagev1.ImageStream)
	if imagestream.Namespace == "openshift-machine-config-operator" {
		glog.Infof("Adding ImageStream %s", imagestream.Name)

		controllerRef := metav1.GetControllerOf(imagestream)
		if controllerRef != nil {
			if pool := ctrl.resolveControllerRef(controllerRef); pool != nil {
				glog.Infof("Imagestream %s changed for %s", imagestream.Name, pool.Name)
				ctrl.enqueueMachineConfigPool(pool)
				return
			}
		}
	}
}

func (ctrl *Controller) updateImageStream(old, cur interface{}) {
	imagestream := cur.(*imagev1.ImageStream)
	if imagestream.Namespace == "openshift-machine-config-operator" {
		glog.Infof("Updating ImageStream %s", imagestream.Name)
		ctrl.addImageStream(cur)
	}
}

func (ctrl *Controller) deleteImageStream(obj interface{}) {
	imagestream := obj.(*imagev1.ImageStream)
	glog.Infof("Deleting ImageStream %s", imagestream.Name)

}
