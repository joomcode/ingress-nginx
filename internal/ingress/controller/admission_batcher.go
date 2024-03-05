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
	batchDelay = 4 * time.Second
)

type AdmissionBatcher struct {
	ingresses     map[Namespace][]*networking.Ingress
	isOnBatching  map[Namespace]bool
	errorChannels map[Namespace][]chan error
	isWorking     bool
	isWorkingMU   *sync.Mutex
	workerMU      *sync.Mutex
	consumerWG    sync.WaitGroup
}

func NewAdmissionBatcher() AdmissionBatcher {
	return AdmissionBatcher{
		ingresses:     map[Namespace][]*networking.Ingress{},
		isOnBatching:  map[Namespace]bool{},
		errorChannels: map[Namespace][]chan error{},
		isWorking:     true,
		isWorkingMU:   &sync.Mutex{},
		workerMU:      &sync.Mutex{},
		consumerWG:    sync.WaitGroup{},
	}
}

func (n *NGINXController) StartAdmissionBatcher() {
	n.admissionBatcher.setWork(true)
	go n.BatchConsumerRoutine()
}

func (n *NGINXController) StopAdmissionBatcher() {
	n.admissionBatcher.setWork(false)
	n.admissionBatcher.consumerWG.Wait()
}

func (n *NGINXController) BatchConsumerRoutine() {
	n.admissionBatcher.consumerWG.Add(1)
	defer n.admissionBatcher.consumerWG.Done()

	for n.admissionBatcher.isWork() {
		ns, ok := n.admissionBatcher.hasNewIngresses()
		if !ok {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		klog.Info("Start waiting for batch, namespace: ", ns)
		go n.BatchConsumerRoutine()
		time.Sleep(batchDelay)
		newIngresses, errorChannels := n.admissionBatcher.fetchNewBatch(ns)

		if len(newIngresses) > 0 {
			err := n.validateNewIngresses(newIngresses)
			for _, erCh := range errorChannels {
				erCh <- err
			}
		}
		return
	}
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

	startTest := time.Now().UnixNano() / 1000000
	start := time.Now()

	_, servers, newIngCfg := n.getConfiguration(ings)
	klog.Info("Configuration generated in ", time.Now().Sub(start).Seconds(), " seconds for ", ingsListStr)

	for _, newIngress := range newIngresses {
		err := checkOverlap(newIngress, servers)
		if err != nil {
			return errors.Wrapf(err, "error while validating overlap for ingress %s/%s", newIngress.Namespace, newIngress.Name)
		}
	}

	start = time.Now()
	template, err := n.generateTemplate(cfg, *newIngCfg)
	if err != nil {
		return errors.Wrapf(err, "error while generating template for ingresses %s", ingsListStr)
	}
	klog.Info("Generated nginx template in ", time.Now().Sub(start).Seconds(), " seconds for ", ingsListStr)

	start = time.Now()
	err = n.testTemplate(template)
	if err != nil {
		return errors.Wrapf(err, "error while testing template for of ingresses %s", ingsListStr)
	}
	klog.Info("Tested nginx template in ", time.Now().Sub(start).Seconds(), " seconds for ", ingsListStr)

	endCheck := time.Now().UnixNano() / 1000000

	testedSize := len(ings)
	confSize := len(template)
	n.metricCollector.SetAdmissionMetrics(
		float64(testedSize),
		float64(endCheck-startTest)/1000,
		float64(len(ings)),
		//can't calculate content properly because of batching
		0,
		float64(confSize),
		//can't calculate content properly because of batching
		0,
	)
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

func (ab *AdmissionBatcher) hasNewIngresses() (Namespace, bool) {
	ab.isWorkingMU.Lock()
	defer ab.isWorkingMU.Unlock()

	for ns, b := range ab.isOnBatching {
		if !b {
			ab.isOnBatching[ns] = true
			return ns, true
		}
	}
	return "", false
}

func (ab *AdmissionBatcher) fetchNewBatch(namespace Namespace) (ings []*networking.Ingress, errorChannels []chan error) {
	ab.isWorkingMU.Lock()
	defer ab.isWorkingMU.Unlock()

	if len(ab.ingresses) == 0 {
		return nil, nil
	}

	ings = ab.ingresses[namespace]
	errorChannels = ab.errorChannels[namespace]

	var sb strings.Builder
	sb.WriteString(fmt.Sprint("Fetched new batch of ingresses for ns: ", namespace))
	for _, ing := range ings {
		sb.WriteString(fmt.Sprintf(" %s", ing.Name))
	}
	klog.Info(sb.String())

	delete(ab.errorChannels, namespace)
	delete(ab.ingresses, namespace)
	delete(ab.isOnBatching, namespace)
	return ings, errorChannels
}

func (ab *AdmissionBatcher) ValidateIngress(ing *networking.Ingress) error {
	errCh := ab.addIngressToHandle(ing)
	klog.Info(
		"Ingress ",
		fmt.Sprintf("%v/%v", ing.Namespace, ing.Name),
		" submitted for batch validation, waiting for verdict...",
	)
	return <-errCh
}

func (ab *AdmissionBatcher) addIngressToHandle(ing *networking.Ingress) chan error {
	ab.isWorkingMU.Lock()
	defer ab.isWorkingMU.Unlock()

	ns := Namespace(ing.Namespace)

	if _, ok := ab.isOnBatching[ns]; !ok {
		ab.isOnBatching[ns] = false
	}
	ab.ingresses[ns] = append(ab.ingresses[ns], ing)

	errCh := make(chan error)
	ab.errorChannels[ns] = append(ab.errorChannels[ns], errCh)
	return errCh
}
