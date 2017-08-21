package networkpolicy

import (
	"reflect"
	"strings"
	"time"
	log "github.com/sirupsen/logrus"
	calicocache "github.com/projectcalico/k8s-policy/pkg/cache"
	"github.com/projectcalico/k8s-policy/pkg/controllers/controller"
	"github.com/projectcalico/k8s-policy/pkg/converter"
	"github.com/projectcalico/libcalico-go/lib/api"
	"github.com/projectcalico/libcalico-go/lib/client"
	"github.com/projectcalico/libcalico-go/lib/errors"
	"k8s.io/apimachinery/pkg/fields"
	uruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/pkg/api/v1"
	"k8s.io/client-go/tools/cache"
	k8sNetV1 "k8s.io/api/networking/v1"
)

// PolicyController Implements Controller interface
// Responsible for monitoring kubernetes network policies and
// syncing them to Calico datastore.
type PolicyController struct {
	indexer        cache.Indexer
	informer       cache.Controller
	calicoObjCache calicocache.ResourceCache
	calicoClient   *client.Client
	k8sClientset   *kubernetes.Clientset
}

// NewPolicyController Constructor for PolicyController
func NewPolicyController(k8sClientset *kubernetes.Clientset, calicoClient *client.Client) controller.Controller {
	policyConverter := converter.NewPolicyConverter()

	// Function returns map of policyName:policy stored by policy controller
	// in datastore.
	listFunc := func() (map[string]interface{}, error) {

		npMap := make(map[string]interface{})

		// Get all policies from datastore
		calicoPolicies, err := calicoClient.Policy().List(api.PolicyMetadata{})
		if err != nil {
			return npMap, err
		}

		// Filter out only objects that are written by policy controller
		for _, policy := range calicoPolicies.Items {
			policyName := policyConverter.GetKey(policy)
			npMap[policyName] = policy
		}

		log.Debugf("Found %d policies in calico datastore:", len(npMap))
		return npMap, nil
	}

	cacheArgs := calicocache.ResourceCacheArgs{
		ListFunc:   listFunc,
		ObjectType: reflect.TypeOf(api.Profile{}),
	}

	ccache := calicocache.NewResourceCache(cacheArgs)

	// create the watcher
	listWatcher := cache.NewListWatchFromClient(k8sClientset.Core().RESTClient(), "networkpolicies", "", fields.Everything())

	// Bind the calico cache to kubernetes cache with the help of an informer. This way we make sure that
	// whenever the kubernetes cache is updated, changes get reflected in calico cache as well.
	indexer, informer := cache.NewIndexerInformer(listWatcher, &k8sNetV1.NetworkPolicy{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			log.Debugf("Got ADD event for network policy: %s\n", key)

			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err != nil {
				log.WithError(err).Error("Failed to generate key")
				return
			}

			policy, err := policyConverter.Convert(obj)
			if err != nil {
				log.WithError(err).Errorf("Error while converting %#v to calico network policy.", obj)
				return
			}

			calicoKey := policyConverter.GetKey(policy)

			// Add policyName:policy in calicoCache
			ccache.Set(calicoKey, policy)
		},
		UpdateFunc: func(oldObj interface{}, newObj interface{}) {
			log.Debugf("Got UPDATE event for network policy: %s\n", key)
			log.Debugf("Old object: %#v\n", oldObj)
			log.Debugf("New object: %#v\n", newObj)

			key, err := cache.MetaNamespaceKeyFunc(newObj)
			if err != nil {
				log.WithError(err).Error("Failed to generate key")
				return
			}

			if newObj.(*k8sNetV1.NetworkPolicy).Status.Phase == "Terminating" {
				// If object status is updated to "Terminating", object
				// is getting deleted. Ignore this event. When deletion
				// completes another DELETE event will be raised.
				// Let DeleteFunc handle that.
				log.Debugf("Network policy %s is getting deleted.", newObj.(*k8sNetV1.NetworkPolicy).ObjectMeta.GetName())
				return
			}

			policy, err := policyConverter.Convert(newObj)
			if err != nil {
				log.WithError(err).Errorf("Error while converting %#v to calico network policy.", obj)
				return
			}

			calicoKey := policyConverter.GetKey(policy)

			// Add policyName:policy in calicoCache
			ccache.Set(calicoKey, policy)
		},
		DeleteFunc: func(obj interface{}) {
			log.Debugf("Got DELETE event for namespace: %s\n", key)

			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err != nil {
				log.WithError(err).Error("Failed to generate key")
				return
			}

			policy, err := policyConverter.Convert(obj)
			if err != nil {
				log.WithError(err).Errorf("Error while converting %#v to calico network policy.", obj)
				return
			}

			calicoKey := policyConverter.GetKey(policy)

			ccache.Delete(calicoKey)
		},
	}, cache.Indexers{})

	return &PolicyController{indexer, informer, ccache, calicoClient, k8sClientset}
}

// Run starts controller.Internally it starts syncing
// kubernetes and calico caches.
func (c *PolicyController) Run(threadiness int, reconcilerPeriod string, stopCh chan struct{}) {
	defer uruntime.HandleCrash()

	// Let the workers stop when we are done
	workqueue := c.calicoObjCache.GetQueue()
	defer workqueue.ShutDown()

	log.Info("Starting network policy controller")

	// Start Calico cache. Cache gets loaded with objects
	// from datastore.
	c.calicoObjCache.Run(reconcilerPeriod)

	go c.informer.Run(stopCh)

	// Wait till k8s cache is synced
	for !c.informer.HasSynced() {
	}

	// Start a number of worker threads to read from the queue.
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	log.Info("Stopping network policy controller")
}

func (c *PolicyController) runWorker() {
	for c.processNextItem() {
	}
}

func (c *PolicyController) processNextItem() bool {

	// Wait until there is a new item in the working queue
	workqueue := c.calicoObjCache.GetQueue()
	key, quit := workqueue.Get()
	if quit {
		return false
	}

	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two nodes with the same key are never processed in
	// parallel.
	defer workqueue.Done(key)

	// Invoke the method containing the business logic
	err := c.syncToCalico(key.(string))

	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key.(string))
	return true
}

// syncToCalico syncs the given update to Calico's datastore, as well as the in-memory cache
// of Calico objects.
func (c *PolicyController) syncToCalico(key string) error {

	// Check if it exists in our cache.
	obj, exists := c.calicoObjCache.Get(key)

	if !exists {
		log.Debugf("Network policy %s does not exist anymore on kubernetes\n", key)
		log.Debugf("Deleting policy %s on datastore \n", key)

		err := c.calicoClient.Policies().Delete(api.PolicyMetadata{
			Name: key,
		})

		// Let Delete() operation be idompotent. Ignore the error while deletion if
		// object does not exists on datastore already.
		if _, ok := err.(errors.ErrorResourceDoesNotExist); ok {
			err = nil
		}
	} else {
		var p api.Policy
		p = obj.(api.Policy)
		log.Infof("Applying network policy %s on datastore \n", key)
		_, err := c.calicoClient.Policies().Apply(&p)
	}
	return err
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *PolicyController) handleErr(err error, key string) {
	workqueue := c.calicoObjCache.GetQueue()
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		workqueue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if workqueue.NumRequeues(key) < 5 {
		log.Errorf("Error syncing network policy %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		workqueue.AddRateLimited(key)
		return
	}

	workqueue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	uruntime.HandleError(err)
	log.Errorf("Dropping network policy %q out of the queue: %v", key, err)
}