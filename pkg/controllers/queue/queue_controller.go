/*
Copyright 2019 The Volcano Authors.

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

package queue

import (
	"fmt"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"

	busv1alpha1 "volcano.sh/volcano/pkg/apis/bus/v1alpha1"
	schedulingv1alpha2 "volcano.sh/volcano/pkg/apis/scheduling/v1alpha2"
	vcclientset "volcano.sh/volcano/pkg/client/clientset/versioned"
	versionedscheme "volcano.sh/volcano/pkg/client/clientset/versioned/scheme"
	informerfactory "volcano.sh/volcano/pkg/client/informers/externalversions"
	busv1alpha1informer "volcano.sh/volcano/pkg/client/informers/externalversions/bus/v1alpha1"
	schedulinginformer "volcano.sh/volcano/pkg/client/informers/externalversions/scheduling/v1alpha2"
	busv1alpha1lister "volcano.sh/volcano/pkg/client/listers/bus/v1alpha1"
	schedulinglister "volcano.sh/volcano/pkg/client/listers/scheduling/v1alpha2"
	queuestate "volcano.sh/volcano/pkg/controllers/queue/state"
)

const (
	// maxRetries is the number of times a queue or command will be retried before it is dropped out of the queue.
	// With the current rate-limiter in use (5ms*2^(maxRetries-1)) the following numbers represent the times
	// a queue or command is going to be requeued:
	// 5ms, 10ms, 20ms, 40ms, 80ms, 160ms, 320ms, 640ms, 1.3s, 2.6s, 5.1s, 10.2s, 20.4s, 41s, 82s
	maxRetries = 15
)

// Controller manages queue status.
type Controller struct {
	kubeClient kubernetes.Interface
	vcClient   vcclientset.Interface

	// informer
	queueInformer schedulinginformer.QueueInformer
	pgInformer    schedulinginformer.PodGroupInformer

	// queueLister
	queueLister schedulinglister.QueueLister
	queueSynced cache.InformerSynced

	// podGroup lister
	pgLister schedulinglister.PodGroupLister
	pgSynced cache.InformerSynced

	cmdInformer busv1alpha1informer.CommandInformer
	cmdLister   busv1alpha1lister.CommandLister
	cmdSynced   cache.InformerSynced

	// queues that need to be updated.
	queue        workqueue.RateLimitingInterface
	commandQueue workqueue.RateLimitingInterface

	pgMutex sync.RWMutex
	// queue name -> podgroup namespace/name
	podGroups map[string]map[string]struct{}

	syncHandler        func(req *schedulingv1alpha2.QueueRequest) error
	syncCommandHandler func(cmd *busv1alpha1.Command) error

	enqueueQueue func(req *schedulingv1alpha2.QueueRequest)

	recorder record.EventRecorder
}

// NewQueueController creates a QueueController
func NewQueueController(
	kubeClient kubernetes.Interface,
	vcClient vcclientset.Interface,
) *Controller {
	factory := informerfactory.NewSharedInformerFactory(vcClient, 0)
	queueInformer := factory.Scheduling().V1alpha2().Queues()
	pgInformer := factory.Scheduling().V1alpha2().PodGroups()

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(klog.Infof)
	eventBroadcaster.StartRecordingToSink(&corev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})

	c := &Controller{
		kubeClient: kubeClient,
		vcClient:   vcClient,

		queueInformer: queueInformer,
		pgInformer:    pgInformer,

		queueLister: queueInformer.Lister(),
		queueSynced: queueInformer.Informer().HasSynced,

		pgLister: pgInformer.Lister(),
		pgSynced: pgInformer.Informer().HasSynced,

		queue:        workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		commandQueue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),

		podGroups: make(map[string]map[string]struct{}),

		recorder: eventBroadcaster.NewRecorder(versionedscheme.Scheme, v1.EventSource{Component: "vc-controllers"}),
	}

	queueInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addQueue,
		UpdateFunc: c.updateQueue,
		DeleteFunc: c.deleteQueue,
	})

	pgInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addPodGroup,
		UpdateFunc: c.updatePodGroup,
		DeleteFunc: c.deletePodGroup,
	})

	c.cmdInformer = informerfactory.NewSharedInformerFactory(c.vcClient, 0).Bus().V1alpha1().Commands()
	c.cmdInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch obj.(type) {
			case *busv1alpha1.Command:
				cmd := obj.(*busv1alpha1.Command)
				return IsQueueReference(cmd.TargetObject)
			default:
				return false
			}
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: c.addCommand,
		},
	})
	c.cmdLister = c.cmdInformer.Lister()
	c.cmdSynced = c.cmdInformer.Informer().HasSynced

	queuestate.SyncQueue = c.syncQueue
	queuestate.OpenQueue = c.openQueue
	queuestate.CloseQueue = c.closeQueue

	c.syncHandler = c.handleQueue
	c.syncCommandHandler = c.handleCommand

	c.enqueueQueue = c.enqueue

	return c
}

// Run starts QueueController
func (c *Controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()
	defer c.commandQueue.ShutDown()

	klog.Infof("Starting queue controller.")
	defer klog.Infof("Shutting down queue controller.")

	go c.queueInformer.Informer().Run(stopCh)
	go c.pgInformer.Informer().Run(stopCh)
	go c.cmdInformer.Informer().Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.queueSynced, c.pgSynced, c.cmdSynced) {
		klog.Errorf("unable to sync caches for queue controller.")
		return
	}

	go wait.Until(c.worker, 0, stopCh)
	go wait.Until(c.commandWorker, 0, stopCh)

	<-stopCh
}

// worker runs a worker thread that just dequeues items, processes them, and
// marks them done. You may run as many of these in parallel as you wish; the
// workqueue guarantees that they will not end up processing the same `queue`
// at the same time.
func (c *Controller) worker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(obj)

	req, ok := obj.(*schedulingv1alpha2.QueueRequest)
	if !ok {
		klog.Errorf("%v is not a valid queue request struct.", obj)
		return true
	}

	err := c.syncHandler(req)
	c.handleQueueErr(err, obj)

	return true
}

func (c *Controller) handleQueue(req *schedulingv1alpha2.QueueRequest) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing queue %s (%v).", req.Name, time.Since(startTime))
	}()

	queue, err := c.queueLister.Get(req.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("Queue %s has been deleted.", req.Name)
			return nil
		}

		return fmt.Errorf("get queue %s failed for %v", req.Name, err)
	}

	queueState := queuestate.NewState(queue)
	if queueState == nil {
		return fmt.Errorf("queue %s state %s is invalid", queue.Name, queue.Status.State)
	}

	if err := queueState.Execute(req.Action); err != nil {
		return fmt.Errorf("sync queue %s failed for %v, event is %v, action is %s",
			req.Name, err, req.Event, req.Action)
	}

	return nil
}

func (c *Controller) handleQueueErr(err error, obj interface{}) {
	if err == nil {
		c.queue.Forget(obj)
		return
	}

	if c.queue.NumRequeues(obj) < maxRetries {
		klog.V(4).Infof("Error syncing queue request %v for %v.", obj, err)
		c.queue.AddRateLimited(obj)
		return
	}

	req, _ := obj.(*schedulingv1alpha2.QueueRequest)
	c.recordEventsForQueue(req.Name, v1.EventTypeWarning, string(req.Action),
		fmt.Sprintf("%v queue failed for %v", req.Action, err))
	klog.V(2).Infof("Dropping queue request %v out of the queue for %v.", obj, err)
	c.queue.Forget(obj)
}

func (c *Controller) commandWorker() {
	for c.processNextCommand() {
	}
}

func (c *Controller) processNextCommand() bool {
	obj, shutdown := c.commandQueue.Get()
	if shutdown {
		return false
	}
	defer c.commandQueue.Done(obj)

	cmd, ok := obj.(*busv1alpha1.Command)
	if !ok {
		klog.Errorf("%v is not a valid Command struct.", obj)
		return true
	}

	err := c.syncCommandHandler(cmd)
	c.handleCommandErr(err, obj)

	return true
}

func (c *Controller) handleCommand(cmd *busv1alpha1.Command) error {
	startTime := time.Now()
	defer func() {
		klog.V(4).Infof("Finished syncing command %s/%s (%v).", cmd.Namespace, cmd.Name, time.Since(startTime))
	}()

	err := c.vcClient.BusV1alpha1().Commands(cmd.Namespace).Delete(cmd.Name, nil)
	if err != nil {
		if true == apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to delete command <%s/%s> for %v", cmd.Namespace, cmd.Name, err)
	}

	req := &schedulingv1alpha2.QueueRequest{
		Name:   cmd.TargetObject.Name,
		Event:  schedulingv1alpha2.QueueCommandIssuedEvent,
		Action: schedulingv1alpha2.QueueAction(cmd.Action),
	}

	c.enqueueQueue(req)

	return nil
}

func (c *Controller) handleCommandErr(err error, obj interface{}) {
	if err == nil {
		c.commandQueue.Forget(obj)
		return
	}

	if c.commandQueue.NumRequeues(obj) < maxRetries {
		klog.V(4).Infof("Error syncing command %v for %v.", obj, err)
		c.commandQueue.AddRateLimited(obj)
		return
	}

	klog.V(2).Infof("Dropping command %v out of the queue for %v.", obj, err)
	c.commandQueue.Forget(obj)
}
