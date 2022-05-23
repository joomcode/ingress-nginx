package controller

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
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
	admissionDelay = 8 * time.Second
)

type AdmissionBatcher struct {
	ingresses     []*networking.Ingress
	errorChannels []chan error

	// flag for consumer goroutine indicating whether it should keep processing or not
	isWorking bool

	// mutex protecting queues access
	mu *sync.Mutex

	// wait group to monitor consumer goroutine lifetime
	consumerWG sync.WaitGroup
}

func NewAdmissionBatcher() AdmissionBatcher {
	return AdmissionBatcher{
		ingresses:     nil,
		errorChannels: nil,
		isWorking:     true,
		mu:            &sync.Mutex{},
		consumerWG:    sync.WaitGroup{},
	}
}

func (ab *AdmissionBatcher) Shutdown() {
	ab.mu.Lock()
	defer ab.mu.Unlock()

	ab.isWorking = false
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
			for _, erCh := range errorChannels {
				erCh <- err
			}
		}

		n.admissionBatcher.mu.Lock()
	}

	klog.Info("Admission batcher routine finished")
}

func groupByNamespacesAndNames(ingresses []*networking.Ingress) map[Namespace]map[Name]struct{} {
	grouped := make(map[Namespace]map[Name]struct{})

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

	//debug
	var ingsListSB strings.Builder
	for _, ing := range newIngresses {
		ingsListSB.WriteString(fmt.Sprintf("%v/%v ", ing.Namespace, ing.Name))
	}
	ingsListStr := ingsListSB.String()

	klog.Info("Similar ingresses filtered for ", ingsListStr)

	annotationsExtractor := annotations.NewAnnotationExtractor(n.store)
	for _, ing := range newIngresses {
		ings = append(ings, &ingress.Ingress{
			Ingress:           *ing,
			ParsedAnnotations: annotationsExtractor.Extract(ing),
		})
	}
	//debug
	klog.Info("New ingresses with annotations appended for ", ingsListStr)

	_, servers, newIngCfg := n.getConfiguration(ings)
	//debug
	klog.Info("Configuration generated for ", ingsListStr)

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
	//debug
	klog.Info("Checked overlapping for ", ingsListStr)

	klog.Info("Generating nginx template with new ingresses: ", ingsListStr)
	template, err := n.generateTemplate(cfg, *newIngCfg)
	if err != nil {
		for _, ing := range newIngresses {
			n.metricCollector.IncCheckErrorCount(ing.ObjectMeta.Namespace, ing.Name)
		}
		return errors.Wrap(err, "error while validating batch of ingresses")
	}

	klog.Info("Testing nginx template with new ingresses: ", ingsListStr)
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

	if len(ab.ingresses) == 0 {
		klog.Info("No new ingresses found by admission batcher routine")
		return nil, nil
	}

	ings = ab.ingresses
	errorChannels = ab.errorChannels

	// debug
	var sb strings.Builder
	sb.WriteString("Fetched new batch of ingresses: ")
	for _, ing := range ings {
		sb.WriteString(fmt.Sprintf("%s/%s ", ing.Namespace, ing.Name))
	}
	klog.Info(sb.String())

	ab.errorChannels = nil
	ab.ingresses = nil

	return ings, errorChannels
}

func (ab *AdmissionBatcher) ValidateIngress(ing *networking.Ingress) error {
	ab.mu.Lock()

	ab.ingresses = append(ab.ingresses, ing)

	errCh := make(chan error)
	ab.errorChannels = append(ab.errorChannels, errCh)

	ab.mu.Unlock()

	// debug
	klog.Info("Ingress ", fmt.Sprintf("%v/%v", ing.Namespace, ing.Name), " submitted for batch validation, waiting for verdict...")
	return <-errCh
}
