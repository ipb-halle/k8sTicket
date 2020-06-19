package k8sfunctions

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/culpinnis/k8sTicket/internal/pkg/proxyfunctions"
	gorilla "github.com/gorilla/mux"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/informers/internalinterfaces"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

//ProxyMap This is a map with a mux that stores the ProxyForDeployments.
// The mux is used by the Informers when adding or deleting (updating)
// a ProxyForDeployment instance.
type ProxyMap struct {
	Deployments map[string]*ProxyForDeployment
	Mux         sync.Mutex
}

//ProxyForDeployment This struct includes everything necessary for running
// the ticket proxy for one deployment.
type ProxyForDeployment struct {
	PodController      *Controller
	Clientset          *kubernetes.Clientset
	Serverlist         *proxyfunctions.Serverlist
	Server             *http.Server
	Router             *gorilla.Router
	Namespace          string
	Port               string
	PodSpec            v1.PodTemplateSpec
	Stopper            chan struct{}
	podWatchdogStopper chan struct{}
	spareTickets       int
	maxPods            int
	cooldown           int
	mux                sync.Mutex
}

//Controller This struct includes all components of the Controller
type Controller struct {
	Clientset interface{} //*kubernetes.Clientset
	Factory   interface{} //informers.SharedInformerFactory
	//We need interfaces here because metadatainformer and infomers
	//are still in different libraries providing different methods.
	//Maybe the implementation will change in the future to a common lib
	Informer cache.SharedInformer
	Stopper  chan struct{}
}

//NewProxyMap Creates a new ProxyMap with initialized Deployment map.
func NewProxyMap() *ProxyMap {
	p := ProxyMap{
		Deployments: make(map[string]*ProxyForDeployment),
	}
	return &p
}

//NewProxyForDeployment The main idea of the k8sTicket structure is that every
// Deployment is one application that should be delivered with the proxy.
// For this reason a proxy controller is created for each deployment.
func NewProxyForDeployment(clienset *kubernetes.Clientset, prefix string, ns string, port string, maxTickets int, spareTickets int, maxPods int, cooldown int, podspec v1.PodTemplateSpec) *ProxyForDeployment {
	proxy := ProxyForDeployment{}
	router := gorilla.NewRouter()
	proxy.Serverlist = proxyfunctions.NewServerlist(prefix)
	proxy.Namespace = ns
	proxy.Port = port
	proxy.PodController = NewPodController(clienset, ns, prefix)
	proxy.PodController.Informer.AddEventHandler(NewPodHandlerForServerlist(proxy.Serverlist, maxTickets))
	proxy.Clientset = clienset
	proxy.PodSpec = podspec
	proxy.Stopper = make(chan struct{})
	proxy.podWatchdogStopper = make(chan struct{})
	proxy.Router = router
	proxy.Server = &http.Server{Addr: ":" + port, Handler: router}
	proxy.spareTickets = spareTickets
	proxy.maxPods = maxPods
	proxy.cooldown = cooldown
	return &proxy
}

//Start This method starts a proxy. That includes the http handler as well as
// the necessary methods and functions to manage tickets.
func (proxy *ProxyForDeployment) Start() {
	//defer close(proxy.PodController.Stopper)
	//defer runtime.HandleCrash()
	log.Println("k8s: ProxyForDeployment: ", proxy.Serverlist.Prefix, " starting...")
	go proxy.PodController.Informer.Run(proxy.PodController.Stopper)
	go proxy.Serverlist.TicketWatchdog()
	go proxy.podScaler(proxy.Serverlist.AddInformerChannel())
	go proxy.podWatchdog()
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix+"/{s}/{serverpath:.*}", proxy.Serverlist.MainHandler)
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix, proxy.Serverlist.ServeHome)
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix+"/", proxy.Serverlist.ServeHome)
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix+"/ws", proxy.Serverlist.ServeWs)
	go func() {
		if err := proxy.Server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("Proxy:", proxy.Serverlist.Prefix, "ListenAndServe()", err)
		}
	}()
}

//Stop This method stops a proxy including the http server and all running
// routines.
func (proxy *ProxyForDeployment) Stop() {
	close(proxy.PodController.Stopper)
	defer runtime.HandleCrash()
	go func() {
		if err := proxy.Server.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Println("HTTP:", proxy.Serverlist.Prefix, "server Shutdown: ", err)
		} else {
			log.Println("HTTP:", proxy.Serverlist.Prefix, "server closed ")
		}
		close(proxy.Stopper)
	}()
	<-proxy.Stopper
	close(proxy.Serverlist.Stop)
	log.Println("Proxy Serverlist:", proxy.Serverlist.Prefix, "closing channles for external functions ")
	for _, channel := range proxy.Serverlist.Informers {
		close(channel)
	}
	log.Println("k8s podWatchdog:", proxy.Serverlist.Prefix, "stopping watchdog ")
	close(proxy.podWatchdogStopper)
}

//NewPodController This function creates a new pod controller for our proxy
// with a InClusterConfig and a given namespace to watch.
func NewPodController(clientset *kubernetes.Clientset, ns string, app string) *Controller {
	factory := informers.NewSharedInformerFactoryWithOptions(clientset,
		1000000000,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(internalinterfaces.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "ipb-halle.de/k8sticket.deployment.app=" + app
		})))
	informer := factory.Core().V1().Pods().Informer()
	stopper := make(chan struct{})
	return (&Controller{
		Clientset: clientset,
		Factory:   factory,
		Informer:  informer,
		Stopper:   stopper,
	})
}

// NewDeploymentController This function creates a new controller for our proxy
// with a Namespace to watch.
func NewDeploymentController(ns string) Controller {
	// creates the in-cluster config
	log.Println("New deployment controller started")
	config, err := rest.InClusterConfig() //I guess it is maybe smarter to give a reference to an exisiting config here instead of generating a new one all the time
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, //I am not sure if the same factory should be used for all controllers
		1000000000,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(internalinterfaces.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "k8sTicket=true"
		})))
	informer := factory.Apps().V1().Deployments().Informer()
	stopper := make(chan struct{})
	return (Controller{
		Clientset: clientset,
		Factory:   factory,
		Informer:  informer,
		Stopper:   stopper,
	})
}

// NewDeploymentMetaController This function creates a new controller for
// the meta data of deployments
func NewDeploymentMetaController(ns string) Controller {
	// creates the in-cluster config
	log.Println("New deployment meta information controller started")
	config, err := rest.InClusterConfig() //I guess it is maybe smarter to give a reference to an exisiting config here instead of generating a new one all the time
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := metadata.NewForConfig(metadata.ConfigFor(config))
	if err != nil {
		panic(err.Error())
	}
	factory := metadatainformer.NewFilteredSharedInformerFactory(clientset, //I am not sure if the same factory should be used for all controllers
		1000000000,
		ns,
		metadatainformer.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "k8sTicket=true"
		}))
	gvr, _ := schema.ParseResourceArg("deployments.v1.apps")
	i := factory.ForResource(*gvr)

	return (Controller{
		Clientset: clientset,
		Factory:   factory,
		Informer:  i.Informer(),
		Stopper:   make(chan struct{}),
	})
}

//NewPodHandlerForServerlist This function creates the PodHandler
// for a given Serverlist. The handler will only handle this specific Serverlist.
// It will create the servers in the Serverlist based on the running pods.
// It will also modify them or delete them if the Pod was modified.
// At the moment there is no use-case for more than one Serverlist, but
// in the future this could happen.
func NewPodHandlerForServerlist(list *proxyfunctions.Serverlist, maxtickets int) cache.ResourceEventHandlerFuncs {
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
		log.Println("k8s: Delete Pod " + pod.Name)
		err := list.SetServerDeletion(pod.Name)
		if err != nil {
			log.Println("k8s: SetServerDeletion:  ", err)
		}
	}
	return (cache.ResourceEventHandlerFuncs{
		AddFunc:    addfunction,
		DeleteFunc: deletefunction,
		UpdateFunc: func(oldObj, newObj interface{}) {
			podOld := oldObj.(*v1.Pod)
			podNew := newObj.(*v1.Pod)
			//We have to check different cases:
			//A server was not ready and can be used now
			//A server was ready but is not ready anymore
			//A server was changed (e.g. IP or other changes)
			if podOld.Status.Phase != v1.PodRunning && podNew.Status.Phase == v1.PodRunning {
				//the pod status was changed to PodRunning
				addfunction(podNew)
			} else if podOld.Status.Phase == v1.PodRunning && podNew.Status.Phase != v1.PodRunning {
				//the pod was changed from PodRunning to sth else
				deletefunction(podNew)
			} else if podOld.Status.PodIP != podNew.Status.PodIP { //I guess this should not happen
				deletefunction(podOld)
				addfunction(podNew)
			} else {
				for conditionOld := range podOld.Status.Conditions {
					if podOld.Status.Conditions[conditionOld].Type == v1.PodReady {
						for conditionNew := range podNew.Status.Conditions {
							if podNew.Status.Conditions[conditionNew].Type == v1.PodReady {
								if podOld.Status.Conditions[conditionOld].Status != v1.ConditionTrue && podNew.Status.Conditions[conditionNew].Status == v1.ConditionTrue {
									addfunction(podNew)
								} else if podOld.Status.Conditions[conditionOld].Status == v1.ConditionTrue && podNew.Status.Conditions[conditionNew].Status != v1.ConditionTrue {
									deletefunction(podNew)
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

//NewDeploymentHandlerForK8sconfig This function creates a new deployment handler.
// It will watch for deployments in k8s with the desired annotations and
// create (delete) the corresponding proxy. It is possible to have more than one
// deployment in a namespace, but they should have different app annotations.
func NewDeploymentHandlerForK8sconfig(c interface{}, ns string, proxies *ProxyMap) cache.ResourceEventHandlerFuncs {
	clientset := c.(*kubernetes.Clientset)
	addfunction := func(obj interface{}) {
		deployment := obj.(*appsv1.Deployment)
		proxies.Mux.Lock()
		log.Println("k8s: Adding deployment" + deployment.Name)
		if _, ok := proxies.Deployments[deployment.Name]; !ok {
			var port string
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]; !ok {
				port = "8080"
			} else {
				port = deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]
			}
			var prefix string
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"]; !ok {
				prefix = deployment.Name
			} else {
				prefix = deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"]
			}
			var maxTickets int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"]; !ok {
				maxTickets = 1
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name + "ipb-halle.de/k8sticket.deployment.maxTickets annotation malformed: " + err.Error())
					maxTickets = 1
				} else {
					maxTickets, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
				}
			}
			var spareTickets int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"]; !ok {
				spareTickets = 2
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name + "ipb-halle.de/k8sticket.deployment.spareTickets annotation malformed: " + err.Error())
					spareTickets = 2
				} else {
					spareTickets, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"])
				}
			}
			var maxPods int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"]; !ok {
				maxPods = 1
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name + "ipb-halle.de/k8sticket.deployment.maxPods annotation malformed: " + err.Error())
					maxPods = 1
				} else {
					maxPods, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"])
				}
			}
			var cooldown int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"]; !ok {
				cooldown = 10
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name + "ipb-halle.de/k8sticket.deployment.Podcooldown annotation malformed: " + err.Error())
					cooldown = 10
				} else {
					cooldown, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"])
				}
			}
			log.Println("k8s: Adding deployment " + deployment.Name + " parameters: ")
			log.Println("k8s: Port: " + port)
			log.Println("k8s: app: " + prefix)
			log.Println("k8s: MaxTickets: ", maxTickets)
			log.Println("k8s: spareTickets: ", spareTickets)
			log.Println("k8s: maxPods: ", maxPods)
			log.Println("k8s: Podcooldown: ", cooldown)
			proxies.Deployments[deployment.Name] = NewProxyForDeployment(clientset, prefix, ns, port, maxTickets, spareTickets, maxPods, cooldown, deployment.Spec.Template)
			proxies.Deployments[deployment.Name].Start()
		} else {
			log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deployment.Name + " already exists!")
		}
		proxies.Mux.Unlock()

	}
	deletionfunction := func(obj interface{}) {
		proxies.Mux.Lock()
		deployment := obj.(*appsv1.Deployment)
		log.Println("k8s: Deleting deployment" + deployment.Name)
		if _, ok := proxies.Deployments[deployment.Name]; !ok {
			log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deployment.Name + " is not known!")
		} else {
			proxies.Deployments[deployment.Name].Stop()
			delete(proxies.Deployments, deployment.Name)
		}
		proxies.Mux.Unlock()
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    addfunction,
		DeleteFunc: deletionfunction,
		UpdateFunc: func(oldObj, newObj interface{}) {
			proxies.Mux.Lock()
			deploymentOld := oldObj.(*appsv1.Deployment)
			deploymentNew := newObj.(*appsv1.Deployment)
			if deploymentNew.Name != deploymentOld.Name {
				log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deploymentOld.Name + " is updated!")
				deletionfunction(deploymentOld)
				addfunction(deploymentNew)
			}
			if deploymentNew.Spec.Template.String() != deploymentOld.Spec.Template.String() {
				log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deploymentOld.Name + " is updated!")
				proxies.Deployments[deploymentNew.Name].mux.Lock()
				proxies.Deployments[deploymentNew.Name].PodSpec = deploymentNew.Spec.Template
				proxies.Deployments[deploymentNew.Name].mux.Unlock()
			}
			//other changes are handled by k8s itself
			//e.g. change of pod template
			proxies.Mux.Unlock()
		},
	}
}

//NewDeploymentHandlerForK8sconfig This function creates a new deployment handler.
// It will watch for deployments in k8s with the desired annotations and
// create (delete) the corresponding proxy. It is possible to have more than one
// deployment in a namespace, but they should have different app annotations.
func NewMetaDeploymentHandlerForK8sconfig(c interface{}, ns string, proxies *ProxyMap) cache.ResourceEventHandlerFuncs {
	clientset := c.(*kubernetes.Clientset)
	return cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			proxies.Mux.Lock()
			deploymentPMetaOld := oldObj.(*metav1.PartialObjectMetadata)
			deploymentPMetaNew := newObj.(*metav1.PartialObjectMetadata)
			deploymentMetaOld := deploymentPMetaOld.ObjectMeta
			deploymentMetaNew := deploymentPMetaNew.ObjectMeta
			ok := true
			//log.Println("k8s: NewMetaDeploymentHandlerForK8sconfig: Deployment " + deploymentMetaOld.Name + " is updated!")
			if deploymentMetaOld.Name != deploymentMetaNew.Name { //I think this is already processed in NewMetaDeploymentHandlerForK8sconfig
				ok = false
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"] {
				ok = false
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"] {
				ok = false
			}
			if !ok { //here we have to restart the proxy
				log.Println("k8s: Deleting deployment" + deploymentMetaOld.Name)
				if _, ok := proxies.Deployments[deploymentMetaOld.Name]; !ok {
					log.Println("k8s: NewMetaDeploymentHandlerForK8sconfig: Deployment " + deploymentMetaOld.Name + " is not known!")
				} else {
					var port string
					if _, ok := deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]; !ok {
						port = "8080"
					} else {
						port = deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]
					}
					var prefix string
					if _, ok := deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"]; !ok {
						prefix = deploymentMetaNew.Name
					} else {
						prefix = deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"]
					}
					var maxTickets int
					_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
					if err != nil {
						log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.maxTickets annotation malformed: " + err.Error())
						maxTickets = 1
					} else {
						maxTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
					}
					log.Println("k8s: Modifying deployment " + deploymentMetaOld.Name + " parameters: ")
					log.Println("k8s: ", deploymentMetaNew.Name, " Port: "+port)
					log.Println("k8s: ", deploymentMetaNew.Name, " app: "+prefix)
					log.Println("k8s: ", deploymentMetaNew.Name, " MaxTickets: ", maxTickets)
					proxies.Deployments[deploymentMetaOld.Name].Stop()
					dpl := proxies.Deployments[deploymentMetaOld.Name]
					delete(proxies.Deployments, deploymentMetaOld.Name)
					proxies.Deployments[deploymentMetaNew.Name] = NewProxyForDeployment(clientset, prefix, ns, port, maxTickets, dpl.spareTickets, dpl.maxPods, dpl.cooldown, dpl.PodSpec)
					proxies.Deployments[deploymentMetaNew.Name].Start()
				}
			} else {
				if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"] {
					_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
					proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
					var maxTickets int
					if err != nil {
						log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.maxTickets annotation malformed: " + err.Error())
						maxTickets = 1
					} else {
						maxTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
					}
					log.Println("k8s: ", deploymentMetaNew.Name, " maxTickets: ", maxTickets)
					proxies.Deployments[deploymentMetaNew.Name].Serverlist.ChangeAllMaxTickets(maxTickets)
					proxies.Deployments[deploymentMetaNew.Name].mux.Unlock()
				}
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"])
				proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.spareTickets annotation malformed: " + err.Error())
					proxies.Deployments[deploymentMetaNew.Name].spareTickets = 2
				} else {
					proxies.Deployments[deploymentMetaNew.Name].spareTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"])
				}
				log.Println("k8s: ", deploymentMetaNew.Name, " spareTickets: ", proxies.Deployments[deploymentMetaNew.Name].spareTickets)
				proxies.Deployments[deploymentMetaNew.Name].mux.Unlock()
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"])
				proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.maxPods annotation malformed: " + err.Error())
					proxies.Deployments[deploymentMetaNew.Name].maxPods = 1
				} else {
					proxies.Deployments[deploymentMetaNew.Name].maxPods, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"])
				}
				log.Println("k8s: ", deploymentMetaNew.Name, " maxPods: ", proxies.Deployments[deploymentMetaNew.Name].maxPods)
				proxies.Deployments[deploymentMetaNew.Name].mux.Unlock()
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"])
				close(proxies.Deployments[deploymentMetaNew.Name].podWatchdogStopper)
				proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
				proxies.Deployments[deploymentMetaNew.Name].podWatchdogStopper = make(chan struct{})
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.Podcooldown annotation malformed: " + err.Error())
					proxies.Deployments[deploymentMetaNew.Name].cooldown = 10
				} else {
					proxies.Deployments[deploymentMetaNew.Name].cooldown, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"])
				}
				log.Println("k8s: ", deploymentMetaNew.Name, " Podcooldown: ", proxies.Deployments[deploymentMetaNew.Name].cooldown)
				proxies.Deployments[deploymentMetaNew.Name].mux.Unlock()
				go proxies.Deployments[deploymentMetaNew.Name].podWatchdog()
			}
			/*	if deploymentMetaNew.GetLabels()["k8sTicket"] != "true" {
				proxies.Deployments[deploymentMetaOld.Name].Stop()
				delete(proxies.Deployments, deploymentMetaOld.Name)
			} */
			//other changes are handled by k8s itself
			//e.g. change of pod template
			proxies.Mux.Unlock()
		},
	}
}

//podScaler This method creates new pods on-demand when a new ticket is created.
func (proxy *ProxyForDeployment) podScaler(informer chan string) {
	for msg := range informer {
		if msg == "new ticket" {
			//check ressources
			proxy.mux.Lock()
			if proxy.Serverlist.GetAvailableTickets() < proxy.spareTickets {
				pods, err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).List(metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
				if err != nil {
					panic(err.Error())
				}
				if len(pods.Items) < proxy.maxPods {
					mypod := v1.Pod{
						ObjectMeta: proxy.PodSpec.ObjectMeta,
						Spec:       proxy.PodSpec.Spec,
					}
					mypod.ObjectMeta.Labels["ipb-halle.de/k8sTicket.scaled"] = "true"
					mypod.GenerateName = strings.ToLower(proxy.Serverlist.Prefix + "-k8sticket-autoscaled-")
					_, err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).Create(&mypod)
					if err != nil {
						panic(err)
					}
					log.Println("k8s: podScaler: Pod created successfully")
				}
			}
			proxy.mux.Unlock()
		}
	}
}

//podWatchdog This method checks if a pod is unused and can be deleted.
// Only pods scaled by the podScaler will be deleted.
func (proxy *ProxyForDeployment) podWatchdog() {
	ticker := time.NewTicker(time.Duration(proxy.cooldown) * time.Second)
	defer ticker.Stop()
	//defer list.mux.Unlock()
	for {
		select {
		case <-ticker.C:
			log.Println("k8s: podWatchdog: Start cleaning")
			pods, err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).List(metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
			if err != nil {
				panic("k8s: podWatchdog: " + err.Error())
			}
			proxy.mux.Lock()
			if proxy.Serverlist.GetAvailableTickets() > proxy.spareTickets {
				for _, pod := range pods.Items {
					if _, ok := proxy.Serverlist.Servers[pod.Name]; ok {
						log.Println("k8s: podWatchdog: Checking "+pod.Name+" with ", len(proxy.Serverlist.Servers[pod.Name].Tickets), " Tickets")
						if proxy.Serverlist.Servers[pod.Name].HasNoTickets() {
							if time.Since(proxy.Serverlist.Servers[pod.Name].GetLastUsed()).Milliseconds() > int64(proxy.cooldown)*time.Second.Milliseconds() {
								if (proxy.Serverlist.GetAvailableTickets() - proxy.Serverlist.Servers[pod.Name].GetMaxTickets()) >= proxy.spareTickets {
									err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).Delete(pod.Name, &metav1.DeleteOptions{})
									if err != nil {
										log.Println("k8s: podWatchdog: Error deleting "+pod.Name+": ", err)
									}
								}
							}
						}
					} else {
						log.Println("k8s: podWatchdog: There is an unused pod which is not in the serverlist " + pod.Name)
						err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).Delete(pod.Name, &metav1.DeleteOptions{})
						if err != nil {
							log.Println("k8s: podWatchdog: Error deleting "+pod.Name+": ", err)
						}
					}
				}
			}
			proxy.mux.Unlock()
		case <-proxy.podWatchdogStopper:
			log.Println("k8s: podWatchdog: Exiting")
			log.Println("k8s: podWatchdog: Start cleaning")
			//The following part can remove autoscaled pods when the ProxyForDeployment is deleted.
			//But this will kill all connections on the running pods, therefore it is disabled.
			/*pods, err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).List(metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
			if err != nil {
				panic("k8s: podWatchdog: " + err.Error())
			}
			proxy.mux.Lock()
			for _, pod := range pods.Items {
				err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).Delete(pod.Name, &metav1.DeleteOptions{})
				if err != nil {
					log.Println("k8s: podWatchdog: Error deleting "+pod.Name+": ", err)
				}
			}
			proxy.mux.Unlock()*/
			return
		}
	}
}
