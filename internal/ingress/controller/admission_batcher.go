package controller

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	networking "k8s.io/api/networking/v1"
	"k8s.io/ingress-nginx/internal/ingress/annotations"
	"k8s.io/ingress-nginx/internal/ingress/controller/store"
	"k8s.io/ingress-nginx/pkg/apis/ingress"
	"k8s.io/klog/v2"
)

type Namespace string
type Name string

const (
	batchDelay    = 5 * time.Second
	batchersCount = 3
)

type AdmissionBatcher struct {
	ingresses     []*networking.Ingress
	errorChannels []chan error
	isWorking     bool
	isWorkingMU   *sync.Mutex
	workerMU      *sync.Mutex
	consumerWG    sync.WaitGroup
}

func NewAdmissionBatcher() AdmissionBatcher {
	return AdmissionBatcher{
		ingresses:     nil,
		errorChannels: nil,
		isWorking:     true,
		isWorkingMU:   &sync.Mutex{},
		workerMU:      &sync.Mutex{},
		consumerWG:    sync.WaitGroup{},
	}
}

func (n *NGINXController) StartAdmissionBatcher() {
	n.admissionBatcher.setWork(true)
	for i := 0; i < batchersCount; i++ {
		go n.BatchConsumerRoutine(i)
	}
}

func (n *NGINXController) StopAdmissionBatcher() {
	n.admissionBatcher.setWork(false)
	n.admissionBatcher.consumerWG.Wait()
}

func (n *NGINXController) BatchConsumerRoutine(i int) {
	n.admissionBatcher.consumerWG.Add(1)
	defer n.admissionBatcher.consumerWG.Done()
	klog.Infof("Admission batcher routine %d started", i)

	for n.admissionBatcher.isWork() {
		n.admissionBatcher.workerMU.Lock()
		if !n.admissionBatcher.hasNewIngresses() {
			n.admissionBatcher.workerMU.Unlock()
			continue
		}

		time.Sleep(batchDelay)
		newIngresses, errorChannels := n.admissionBatcher.fetchNewBatch()
		n.admissionBatcher.workerMU.Unlock()

		if len(newIngresses) > 0 {
			err := n.validateNewIngresses(newIngresses)
			for _, erCh := range errorChannels {
				erCh <- err
			}
		}
	}
	klog.Infof("Admission batcher routine %d finished", i)
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

	var ingsListSB strings.Builder
	for _, ing := range newIngresses {
		ingsListSB.WriteString(fmt.Sprintf("%v/%v ", ing.Namespace, ing.Name))
	}
	ingsListStr := ingsListSB.String()

	annotationsExtractor := annotations.NewAnnotationExtractor(n.store)
	for _, ing := range newIngresses {
		ann, err := annotationsExtractor.Extract(ing)
		if err != nil {
			return err
		}
		ings = append(ings, &ingress.Ingress{
			Ingress:           *ing,
			ParsedAnnotations: ann,
		})
	}

	klog.Info("New ingresses with annotations appended for ", ingsListStr)

	start := time.Now()
	_, _, newIngCfg := n.getConfiguration(ings)
	klog.Info("Configuration generated in ", time.Now().Sub(start).Seconds(), " seconds for ", ingsListStr)

	start = time.Now()
	template, err := n.generateTemplate(cfg, *newIngCfg)
	if err != nil {
		return errors.Wrap(err, "error while validating batch of ingresses")
	}
	klog.Info("Generated nginx template in ", time.Now().Sub(start).Seconds(), " seconds for ", ingsListStr)

	start = time.Now()
	err = n.testTemplate(template)
	if err != nil {
		return errors.Wrap(err, "error while validating batch of ingresses")
	}
	klog.Info("Tested nginx template in ", time.Now().Sub(start).Seconds(), " seconds for ", ingsListStr)

	return nil
}

func (ab *AdmissionBatcher) setWork(status bool) {
	ab.isWorkingMU.Lock()
	defer ab.isWorkingMU.Unlock()

	ab.isWorking = status
}

func (ab *AdmissionBatcher) isWork() bool {
	ab.isWorkingMU.Lock()
	defer ab.isWorkingMU.Unlock()

	return ab.isWorking
}

func (ab *AdmissionBatcher) hasNewIngresses() bool {
	ab.isWorkingMU.Lock()
	defer ab.isWorkingMU.Unlock()

	return len(ab.ingresses) != 0
}

func (ab *AdmissionBatcher) fetchNewBatch() (ings []*networking.Ingress, errorChannels []chan error) {
	ab.isWorkingMU.Lock()
	defer ab.isWorkingMU.Unlock()

	if len(ab.ingresses) == 0 {
		return nil, nil
	}

	ings = ab.ingresses
	errorChannels = ab.errorChannels

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
	ab.isWorkingMU.Lock()

	ab.ingresses = append(ab.ingresses, ing)

	errCh := make(chan error)
	ab.errorChannels = append(ab.errorChannels, errCh)

	ab.isWorkingMU.Unlock()

	klog.Info("Ingress ", fmt.Sprintf("%v/%v", ing.Namespace, ing.Name), " submitted for batch validation, waiting for verdict...")
	return <-errCh
}
