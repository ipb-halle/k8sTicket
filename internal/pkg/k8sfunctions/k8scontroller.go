package k8sfunctions

import (
	"fmt"
	"log"

	"github.com/culpinnis/k8sTicket/internal/pkg/proxyfunctions"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/informers/internalinterfaces"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

// Controller This struct includes a components of the Controller
type Controller struct {
	clientset kubernetes.Interface
	factory   informers.SharedInformerFactory
	Informer  cache.SharedInformer
	Stopper   chan struct{}
}

// New_proxy_controller This function makes a new controller for our proxy
// with a InClusterConfig and a given namespace to watch.
func New_proxy_controller(ns string) Controller {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	factory := informers.NewSharedInformerFactoryWithOptions(clientset,
		0,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(internalinterfaces.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "k8sTicket=true"
		})))
	informer := factory.Core().V1().Pods().Informer()
	stopper := make(chan struct{})
	return (Controller{
		clientset: clientset,
		factory:   factory,
		Informer:  informer,
		Stopper:   stopper,
	})
}

// New_handler_for_serverlist This function creates the k8sTicket handler
// for a given Servlist. The handler will only handle this specific Serverlist.
// At the moment there is no use-case for more than one Serverlist, but
// in the future this could happen.
func New_handler_for_serverlist(list *proxyfunctions.Serverlist, maxtickets int) cache.ResourceEventHandlerFuncs {
	addfunction := func(obj interface{}) {
		pod := obj.(*v1.Pod)
		if pod.Status.Phase == v1.PodRunning {
			var rdy = true
			for condition := range pod.Status.Conditions {
				if pod.Status.Conditions[condition].Type == v1.PodReady {
					if pod.Status.Conditions[condition].Status != v1.ConditionTrue {
						rdy = false
					}
				}
			}
			if rdy {
				log.Println("k8s: New Pod " + pod.Name)
				conf, err := PodToConfig(pod)
				if err == nil {
					err := list.AddServer(pod.Name, maxtickets, conf)
					if err != nil {
						log.Println("k8s: AddServer:  ", err)
					}
				}
			}
		}
	}
	deletefunction := func(obj interface{}) {
		pod := obj.(*v1.Pod)
		fmt.Println("k8s: Delete Pod " + pod.Name)
		err := list.SetServerDeletion(pod.Name)
		if err != nil {
			log.Println("k8s: SetServerDeletion:  ", err)
		}
	}
	return (cache.ResourceEventHandlerFuncs{
		AddFunc:    addfunction,
		DeleteFunc: deletefunction,
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod_old := oldObj.(*v1.Pod)
			pod_new := newObj.(*v1.Pod)
			//We have to check different cases:
			//A server was not ready and can be used now
			//A server was ready but is not ready anymore
			//A server was changed (e.g. IP or other changes)
			if pod_old.Status.Phase != v1.PodRunning && pod_new.Status.Phase == v1.PodRunning {
				//the pod status was changed to PodRunning
				addfunction(pod_new)
			} else if pod_old.Status.Phase == v1.PodRunning && pod_new.Status.Phase != v1.PodRunning {
				//the pod was changed from PodRunning to sth else
				deletefunction(pod_new)
			} else if pod_old.Status.PodIP != pod_new.Status.PodIP {
				deletefunction(pod_old)
				addfunction(pod_new)
			} else {
				for condition_old := range pod_old.Status.Conditions {
					if pod_old.Status.Conditions[condition_old].Type == v1.PodReady {
						for condition_new := range pod_new.Status.Conditions {
							if pod_new.Status.Conditions[condition_new].Type == v1.PodReady {
								if pod_old.Status.Conditions[condition_old].Status != v1.ConditionTrue && pod_new.Status.Conditions[condition_new].Status == v1.ConditionTrue {
									addfunction(pod_new)
								} else if pod_old.Status.Conditions[condition_old].Status == v1.ConditionTrue && pod_new.Status.Conditions[condition_new].Status != v1.ConditionTrue {
									deletefunction(pod_new)
								}
							}
						}
					}
				}
				//I think all other changes will not affect the proxy service
			}
		},
	})
}
