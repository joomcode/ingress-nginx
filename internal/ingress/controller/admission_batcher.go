package controller

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/atomic"
	networking "k8s.io/api/networking/v1beta1"
	"k8s.io/ingress-nginx/internal/ingress"
	"k8s.io/ingress-nginx/internal/ingress/annotations"
	"k8s.io/ingress-nginx/internal/ingress/controller/store"
	"k8s.io/klog/v2"
)

type Namespace string
type Name string

const (
	// time of batch collecting
	admissionDelay = 5 * time.Second
)

type AdmissionQueue struct {
	// buffered channel of ingresses awaiting to be processed
	ingresses     []*networking.Ingress
	errorChannels []chan error
}

func NewAdmissionQueue() AdmissionQueue {
	return AdmissionQueue{
		ingresses:     nil,
		errorChannels: nil,
	}
}

type AdmissionBatcher struct {
	queues [2]AdmissionQueue

	// index of queue that is not being processed right now
	freeQueueIdx atomic.Int32

	// flag for consumer goroutine indicating whether it should keep processing or not
	isWorking bool

	// mutex protecting queues access
	mu *sync.Mutex

	// wait group to monitor consumer goroutine lifetime
	consumerWG sync.WaitGroup
}

func NewAdmissionBatcher() AdmissionBatcher {
	return AdmissionBatcher{
		queues:       [2]AdmissionQueue{NewAdmissionQueue(), NewAdmissionQueue()},
		freeQueueIdx: atomic.Int32{},
		isWorking:    true,
		mu:           &sync.Mutex{},
		consumerWG:   sync.WaitGroup{},
	}
}

func (ab *AdmissionBatcher) Shutdown() {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	ab.isWorking = false
}

func (ab *AdmissionBatcher) freeQueue() AdmissionQueue {
	return ab.queues[ab.freeQueueIdx.Load()]
}

func (ab *AdmissionBatcher) nextFreeQueue() AdmissionQueue {
	return ab.queues[1-ab.freeQueueIdx.Load()]
}

func (ab *AdmissionBatcher) swapQueues() {
	ab.freeQueueIdx.Store(1 - ab.freeQueueIdx.Load())
}

// AdmissionBatcherConsumerRoutine is started during ingress-controller startup phase
// And it should stop during ingress-controller's graceful shutdown
func (n *NGINXController) AdmissionBatcherConsumerRoutine() {
	n.admissionBatcher.consumerWG.Add(1)
	defer n.admissionBatcher.consumerWG.Done()

	klog.Info("Admission batcher routine started")

	// prevent races on isWorking field
	n.admissionBatcher.mu.Lock()
	for n.admissionBatcher.isWorking {
		n.admissionBatcher.mu.Unlock()

		time.Sleep(admissionDelay)
		newIngresses, errorChannels := n.admissionBatcher.fetchNewBatch()
		if len(newIngresses) != 0 {
			var logmsg strings.Builder
			logmsg.WriteString("Received new batch of ingresses for validation: ")
			for _, ing := range newIngresses {
				logmsg.WriteString(fmt.Sprintf("%s/%s ", ing.ObjectMeta.Namespace, ing.ObjectMeta.Name))
			}
			klog.Info(logmsg.String())

			err := n.validateNewIngresses(newIngresses)
			if err != nil {
				for _, erCh := range errorChannels {
					erCh <- err
				}
			}
		}

		n.admissionBatcher.mu.Lock()
	}
}

func groupByNamespacesAndNames(ingresses []*networking.Ingress) map[Namespace]map[Name]struct{} {
	var grouped map[Namespace]map[Name]struct{}

	for _, ing := range ingresses {
		ns := Namespace(ing.ObjectMeta.Namespace)
		name := Name(ing.ObjectMeta.Name)
		if _, exists := grouped[ns]; !exists {
			grouped[ns] = make(map[Name]struct{})
		}

		grouped[ns][name] = struct{}{}
	}

	return grouped
}

func (n *NGINXController) validateNewIngresses(newIngresses []*networking.Ingress) error {
	cfg := n.store.GetBackendConfiguration()
	cfg.Resolver = n.resolver

	newIngsDict := groupByNamespacesAndNames(newIngresses)

	allIngresses := n.store.ListIngresses()
	ings := store.FilterIngresses(allIngresses, func(toCheck *ingress.Ingress) bool {
		ns := Namespace(toCheck.ObjectMeta.Namespace)
		name := Name(toCheck.ObjectMeta.Name)

		nsNames, nsMatchesAny := newIngsDict[ns]
		if !nsMatchesAny {
			return false
		}
		_, nameMatchesAny := nsNames[name]
		return nameMatchesAny
	})

	for _, ing := range newIngresses {
		ings = append(ings, &ingress.Ingress{
			Ingress:           *ing,
			ParsedAnnotations: annotations.NewAnnotationExtractor(n.store).Extract(ing),
		})
	}

	_, servers, newIngCfg := n.getConfiguration(ings)

	var err error
	for _, ing := range newIngresses {
		err = checkOverlap(ing, servers)
		if err != nil {
			for _, ing := range newIngresses {
				n.metricCollector.IncCheckErrorCount(ing.ObjectMeta.Namespace, ing.Name)
			}
			return errors.Wrap(err, "error while validating batch of ingresses")
		}
	}

	template, err := n.generateTemplate(cfg, *newIngCfg)
	if err != nil {
		for _, ing := range newIngresses {
			n.metricCollector.IncCheckErrorCount(ing.ObjectMeta.Namespace, ing.Name)
		}
		return errors.Wrap(err, "error while validating batch of ingresses")
	}

	err = n.testTemplate(template)
	if err != nil {
		for _, ing := range newIngresses {
			n.metricCollector.IncCheckErrorCount(ing.ObjectMeta.Namespace, ing.Name)
		}
		return errors.Wrap(err, "error while validating batch of ingresses")
	}

	for _, ing := range newIngresses {
		n.metricCollector.IncCheckCount(ing.ObjectMeta.Namespace, ing.Name)
	}

	return nil
}

func (ab *AdmissionBatcher) fetchNewBatch() (ings []*networking.Ingress, errorChannels []chan error) {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	freeQueue := ab.freeQueue()

	ings = freeQueue.ingresses
	errorChannels = freeQueue.errorChannels

	freeQueue.errorChannels = nil
	freeQueue.ingresses = nil

	ab.swapQueues()

	return ings, errorChannels
}

func (ab *AdmissionBatcher) ValidateIngress(ing *networking.Ingress) error {
	ab.mu.Lock()
	freeQueue := ab.freeQueue()
	freeQueue.ingresses = append(freeQueue.ingresses, ing)

	errCh := make(chan error)
	freeQueue.errorChannels = append(freeQueue.errorChannels, errCh)

	return <-errCh
}
