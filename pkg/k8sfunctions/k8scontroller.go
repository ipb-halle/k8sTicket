package k8sfunctions

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gorilla "github.com/gorilla/mux"
	"github.com/ipb-halle/k8sTicket/pkg/proxyfunctions"
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

//ProxyForDeployment This struct includes everything needed for running
// the ticket proxy for one deployment. It is the essiential structure of k8sTicket.
type ProxyForDeployment struct {
	podController      *Controller
	Clientset          *kubernetes.Clientset
	Serverlist         *proxyfunctions.Serverlist
	server             *http.Server
	router             *gorilla.Router
	namespace          string
	port               string
	podSpec            v1.PodTemplateSpec
	Stopper            chan struct{}
	podWatchdogStopper chan struct{}
	podScalerStopper   chan struct{}
	podScalerInformer  chan string
	metricStopper      chan struct{}
	spareTickets       int
	maxPods            int
	cooldown           int
	mux                sync.Mutex
	metric             *PMetric
	dns                bool
}

//Controller This struct includes all components of the Controller
// It has a clientset, a Informer.SharedInformerFactory, the Informer that
// was created by the Factory and a Channel stop the informer.
type Controller struct {
	Clientset interface{} //*kubernetes.Clientset
	Factory   interface{} //informers.SharedInformerFactory
	//We need interfaces here because metadatainformer and infomers
	//are still in different libraries providing different methods.
	//Maybe the implementation will change in the future to a common lib
	Informer cache.SharedInformer
	Stopper  chan struct{}
}

//NewProxyMap Creates a new ProxyMap with initialized ProxyForDeployment map.
func NewProxyMap() *ProxyMap {
	p := ProxyMap{
		Deployments: make(map[string]*ProxyForDeployment),
	}
	return &p
}

//NewProxyForDeployment The main idea of the k8sTicket structure is that every
// Deployment is one application that should be delivered with the proxy.
// For this reason all components are tied together in this structure.
func NewProxyForDeployment(clienset *kubernetes.Clientset, prefix string, ns string,
	port string, maxTickets int, spareTickets int, maxPods int, cooldown int,
	podspec v1.PodTemplateSpec, metric *PMetric, dns bool) *ProxyForDeployment {

	proxy := ProxyForDeployment{}
	router := gorilla.NewRouter()
	proxy.Serverlist = proxyfunctions.NewServerlist(prefix, dns)
	proxy.namespace = ns
	proxy.port = port
	proxy.podController = NewPodController(clienset, ns, prefix)
	proxy.podController.Informer.AddEventHandler(NewPodHandlerForServerlist(&proxy, maxTickets))
	proxy.Clientset = clienset
	proxy.podSpec = podspec
	proxy.Stopper = make(chan struct{})
	proxy.podWatchdogStopper = make(chan struct{})
	proxy.podScalerInformer = proxy.Serverlist.AddInformerChannel()
	proxy.podScalerStopper = make(chan struct{})
	proxy.metricStopper = make(chan struct{})
	proxy.router = router
	proxy.server = &http.Server{Addr: ":" + port, Handler: router}
	proxy.spareTickets = spareTickets
	proxy.maxPods = maxPods
	proxy.cooldown = cooldown
	proxy.metric = metric
	proxy.dns = dns
	return &proxy
}

//Start This method starts a proxy. That includes the http handler as well as
// the necessary methods and functions to manage tickets. Furthermore,
// k8s informers are started to watch the events in the cluster.
func (proxy *ProxyForDeployment) Start() {
	//defer close(proxy.podController.Stopper)
	//defer runtime.HandleCrash()
	log.Println("k8s: ProxyForDeployment: ", proxy.Serverlist.Prefix, " starting...")
	go proxy.podController.Informer.Run(proxy.podController.Stopper)
	go proxy.Serverlist.TicketWatchdog()
	go proxy.podScaler()
	go proxy.podWatchdog()
	proxy.router.HandleFunc("/"+proxy.Serverlist.Prefix+"/ws", proxy.Serverlist.ServeWs)
	if proxy.dns {
		proxy.router.HandleFunc("/"+proxy.Serverlist.Prefix, proxy.Serverlist.MainHandler).Host("{s}.{u}.{domain:.*}")
		proxy.router.HandleFunc("/"+proxy.Serverlist.Prefix+"/{serverpath:.*}", proxy.Serverlist.MainHandler).Host("{s}.{u}.{domain:.*}")
	} else {
		proxy.router.HandleFunc("/"+proxy.Serverlist.Prefix+"/{s}/{u}/{serverpath:.*}", proxy.Serverlist.MainHandler)
	}
	proxy.router.HandleFunc("/"+proxy.Serverlist.Prefix, proxy.Serverlist.ServeHome)
	proxy.router.HandleFunc("/"+proxy.Serverlist.Prefix+"/", proxy.Serverlist.ServeHome)

	go func() {
		if err := proxy.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Println("Proxy:", proxy.Serverlist.Prefix, "ListenAndServe()", err)
		}
	}()
	go proxy.UpdateAccessMetric(proxy.Serverlist.AddInformerChannel())
}

//Stop This method stops a proxy including the http server and all running
// routines and infomers.
func (proxy *ProxyForDeployment) Stop() {
	close(proxy.podController.Stopper)
	defer runtime.HandleCrash()
	go func() {
		if err := proxy.server.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Println("HTTP:", proxy.Serverlist.Prefix, "server Shutdown: ", err)
		} else {
			log.Println("HTTP:", proxy.Serverlist.Prefix, "server closed ")
		}
		close(proxy.Stopper)
	}()
	<-proxy.Stopper
	close(proxy.Serverlist.Stop)
	close(proxy.podScalerStopper)
	close(proxy.metricStopper)
	log.Println("Proxy Serverlist:", proxy.Serverlist.Prefix, "closing channles for external functions ")
	proxy.Serverlist.Mux.Lock()
	for _, channel := range proxy.Serverlist.Informers {
		close(channel)
	}
	proxy.Serverlist.Mux.Unlock()
	log.Println("k8s podWatchdog:", proxy.Serverlist.Prefix, "stopping watchdog ")
	close(proxy.podWatchdogStopper)
}

//NewPodController This function creates a new Pod controller for a proxy
// with a kubernetes.Clientset and a given namespace to watch.
// It will inform k8sTicket about creation, deletion or updates of running Pods for the parameter app.
func NewPodController(clientset kubernetes.Interface, ns string, app string) *Controller {
	factory := informers.NewSharedInformerFactoryWithOptions(clientset,
		1000000000,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(internalinterfaces.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "ipb-halle.de/k8sticket.deployment.app.name=" + app
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

// NewDeploymentController This function creates a new Deployment controller for a proxy
// with a hardcoded InClusterConfig and a given namespace to watch.
// It will inform k8sTicket about creation, deletion or updates of running Deployments.
// This controller is a major component because k8sTicket will retrieve its configuration
// from the Deployments.
func NewDeploymentController(ns string) Controller {
	// creates the in-cluster config
	log.Println("New deployment controller started")
	config, err := rest.InClusterConfig() //I guess it is maybe smarter to give a
	//reference to an exisiting config here instead of generating a new one all the time
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	factory := informers.NewSharedInformerFactoryWithOptions(clientset, //I am not
		//sure if the same factory should be used for all controllers
		1000000000,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(internalinterfaces.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "ipb-halle.de/k8sticket=true"
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
// the meta data of Deployments. In that way it is a kind of extension of the
// DeploymentController which only listens for Deployments themselves.
// This controlller handels the updates of Annotations and Labels.
func NewDeploymentMetaController(ns string) Controller {
	// creates the in-cluster config
	log.Println("New deployment meta information controller started")
	config, err := rest.InClusterConfig() //I guess it is maybe smarter to give a
	//reference to an exisiting config here instead of generating a new one all the time
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := metadata.NewForConfig(metadata.ConfigFor(config))
	if err != nil {
		panic(err.Error())
	}
	factory := metadatainformer.NewFilteredSharedInformerFactory(clientset, //I am not
		// sure if the same factory should be used for all controllers
		1000000000,
		ns,
		metadatainformer.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "ipb-halle.de/k8sticket=true"
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
// for a given Proxy. The handler will only handle the specific Serverlist of this Proxy.
// It will create the servers in the Serverlist based on the running Pods.
// It will also modify them or delete them if the Pod was modified.
// This handler implements the actions of the podController and triggers the UpdatePodMetric method.
func NewPodHandlerForServerlist(proxy *ProxyForDeployment, maxtickets int) cache.ResourceEventHandlerFuncs {
	list := proxy.Serverlist
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
		proxy.UpdatePodMetric()
	}
	deletefunction := func(obj interface{}) {
		pod := obj.(*v1.Pod)
		log.Println("k8s: Delete Pod " + pod.Name)
		err := list.SetServerDeletion(pod.Name)
		if err != nil {
			log.Println("k8s: SetServerDeletion:  ", err)
		}
		proxy.UpdatePodMetric()
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
								if podOld.Status.Conditions[conditionOld].Status != v1.ConditionTrue &&
									podNew.Status.Conditions[conditionNew].Status == v1.ConditionTrue {

									addfunction(podNew)

								} else if podOld.Status.Conditions[conditionOld].Status == v1.ConditionTrue &&
									podNew.Status.Conditions[conditionNew].Status != v1.ConditionTrue {

									deletefunction(podNew)
								}
							}
						}
					}
				}
				//I think all other changes will not affect the proxy service
			}
			proxy.UpdatePodMetric()
		},
	})
}

//NewDeploymentHandlerForK8sconfig This function creates a new deployment handler.
// It will watch for deployments in k8s with the desired annotations and
// create (delete) the corresponding proxy. It is possible to have more than one
// Deployment in a namespace, but they should have different app annotations and different ports.
// This handler implements the actions of the DeploymentController.
func NewDeploymentHandlerForK8sconfig(c interface{}, ns string,
	proxies *ProxyMap, metric *PMetric) cache.ResourceEventHandlerFuncs {
	clientset := c.(*kubernetes.Clientset)
	addfunction := func(obj interface{}) {
		deployment := obj.(*appsv1.Deployment)
		proxies.Mux.Lock()
		log.Println("k8s: Adding deployment" + deployment.Name)
		if _, ok := proxies.Deployments[deployment.Name]; !ok {
			var port string
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]; !ok {
				port = "9001"
			} else {
				port = deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]
			}
			var prefix string
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app.name"]; !ok {
				prefix = deployment.Name
			} else {
				prefix = deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app.name"]
			}
			var maxTickets int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"]; !ok {
				maxTickets = 1
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name +
						"ipb-halle.de/k8sticket.deployment.tickets.max annotation malformed: " + err.Error())
					maxTickets = 1
				} else {
					maxTickets, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"])
				}
			}
			var spareTickets int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.spare"]; !ok {
				spareTickets = 2
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.spare"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name +
						"ipb-halle.de/k8sticket.deployment.tickets.spare annotation malformed: " + err.Error())
					spareTickets = 2
				} else {
					spareTickets, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.spare"])
				}
			}
			var maxPods int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.max"]; !ok {
				maxPods = 1
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.max"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name +
						"ipb-halle.de/k8sticket.deployment.pods.max annotation malformed: " + err.Error())
					maxPods = 1
				} else {
					maxPods, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.max"])
				}
			}
			var cooldown int
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.cooldown"]; !ok {
				cooldown = 10
			} else {
				_, err := strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.cooldown"])
				if err != nil {
					log.Println("k8s: Deployment: " + deployment.Name +
						"ipb-halle.de/k8sticket.deployment.pods.cooldown annotation malformed: " + err.Error())
					cooldown = 10
				} else {
					cooldown, _ = strconv.Atoi(deployment.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.cooldown"])
				}
			}
			var dns bool = false
			if _, ok := deployment.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"]; ok {
				_, err := strconv.ParseBool(deployment.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"])
				if err == nil {
					dns, _ = strconv.ParseBool(deployment.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"])
				} else {
					log.Println("k8s: Deployment: " + deployment.Name + "ipb-halle.de/k8sticket.ingress.dns annotation malformed: " + err.Error())
				}
			}

			log.Println("k8s: Adding deployment " + deployment.Name + " parameters: ")
			log.Println("k8s: port: " + port)
			log.Println("k8s: app: " + prefix)
			log.Println("k8s: tickets.max: ", maxTickets)
			log.Println("k8s: tickets.spare: ", spareTickets)
			log.Println("k8s: pod.max: ", maxPods)
			log.Println("k8s: pod.cooldown: ", cooldown)
			log.Println("k8s: ingress.dns: ", dns)
			proxies.Deployments[deployment.Name] = NewProxyForDeployment(clientset, prefix,
				ns, port, maxTickets, spareTickets, maxPods, cooldown, deployment.Spec.Template, metric, dns)
			proxies.Deployments[deployment.Name].Start()
		} else {
			log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deployment.Name + " already exists!")
		}
		proxies.Mux.Unlock()

	}
	deletionfunction := func(obj interface{}) {
		proxies.Mux.Lock()
		deployment := obj.(*appsv1.Deployment)
		log.Println("k8s: Deleting deployment " + deployment.Name)
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
			proxies.Deployments[deploymentNew.Name].mux.Lock()
			if deploymentNew.Spec.Template.String() != deploymentOld.Spec.Template.String() {
				log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deploymentOld.Name + " is updated!")
				proxies.Deployments[deploymentNew.Name].podSpec = deploymentNew.Spec.Template
			}
			proxies.Deployments[deploymentNew.Name].mux.Unlock()

			//other changes are handled by k8s itself
			//e.g. change of pod template
			proxies.Mux.Unlock()
		},
	}
}

//NewMetaDeploymentHandlerForK8sconfig This function creates a new handler for the meta data of Deployments.
// It will only watch for updates of the meta data.
// It does basically the same job as the handler for the Deployment,
// but enables the change of parameters while the application is running.
// This handler implements the actions of the DeploymentMetaController.
func NewMetaDeploymentHandlerForK8sconfig(c interface{}, ns string,
	proxies *ProxyMap, metric *PMetric) cache.ResourceEventHandlerFuncs {
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
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app.name"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app.name"] {
				ok = false
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"] {
				ok = false
			}
			if !ok { //here we have to restart the proxy
				log.Println("k8s: Deleting deployment " + deploymentMetaOld.Name)
				if _, ok := proxies.Deployments[deploymentMetaOld.Name]; !ok {
					log.Println("k8s: NewMetaDeploymentHandlerForK8sconfig: Deployment " + deploymentMetaOld.Name + " is not known!")
				} else {
					var port string
					if _, ok := deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]; !ok {
						port = "9001"
					} else {
						port = deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"]
					}
					var prefix string
					if _, ok := deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app.name"]; !ok {
						prefix = deploymentMetaNew.Name
					} else {
						prefix = deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app.name"]
					}
					var maxTickets int
					_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"])
					if err != nil {
						log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.tickets.max annotation malformed: " + err.Error())
						maxTickets = 1
					} else {
						maxTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"])
					}
					var dns bool = false
					if _, ok := deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"]; ok {
						_, err := strconv.ParseBool(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"])
						if err == nil {
							dns, _ = strconv.ParseBool(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.ingress.dns"])
						} else {
							log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.ingress.dns annotation malformed: " + err.Error())
						}
					}
					log.Println("k8s: Modifying deployment " + deploymentMetaOld.Name + " parameters: ")
					log.Println("k8s: ", deploymentMetaNew.Name, " port: "+port)
					log.Println("k8s: ", deploymentMetaNew.Name, " app: "+prefix)
					log.Println("k8s: ", deploymentMetaNew.Name, " tickets.max: ", maxTickets)
					log.Println("k8s: ", deploymentMetaNew.Name, " ingress.dns: ", dns)
					proxies.Deployments[deploymentMetaOld.Name].Stop()
					dpl := proxies.Deployments[deploymentMetaOld.Name]
					delete(proxies.Deployments, deploymentMetaOld.Name)
					proxies.Deployments[deploymentMetaNew.Name] = NewProxyForDeployment(clientset, prefix,
						ns, port, maxTickets, dpl.spareTickets, dpl.maxPods, dpl.cooldown, dpl.podSpec, metric, dns)
					proxies.Deployments[deploymentMetaNew.Name].Start()
				}
			} else {
				if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"] {
					_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"])
					proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
					var maxTickets int
					if err != nil {
						log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.tickets.max annotation malformed: " + err.Error())
						maxTickets = 1
					} else {
						maxTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.max"])
					}
					log.Println("k8s: ", deploymentMetaNew.Name, " tickets.max: ", maxTickets)
					proxies.Deployments[deploymentMetaNew.Name].Serverlist.ChangeAllMaxTickets(maxTickets)
					proxies.Deployments[deploymentMetaNew.Name].mux.Unlock()
				}
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.spare"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.spare"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.spare"])
				proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.tickets.spare annotation malformed: " + err.Error())
					proxies.Deployments[deploymentMetaNew.Name].spareTickets = 2
				} else {
					proxies.Deployments[deploymentMetaNew.Name].spareTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.tickets.spare"])
				}
				log.Println("k8s: ", deploymentMetaNew.Name, " spareTickets: ", proxies.Deployments[deploymentMetaNew.Name].spareTickets)
				proxies.Deployments[deploymentMetaNew.Name].mux.Unlock()
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.max"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.max"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.max"])
				proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.pods.max annotation malformed: " + err.Error())
					proxies.Deployments[deploymentMetaNew.Name].maxPods = 1
				} else {
					proxies.Deployments[deploymentMetaNew.Name].maxPods, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.max"])
				}
				proxies.Deployments[deploymentMetaNew.Name].podScalerInformer <- "update"
				log.Println("k8s: ", deploymentMetaNew.Name, " maxPods: ", proxies.Deployments[deploymentMetaNew.Name].maxPods)
				proxies.Deployments[deploymentMetaNew.Name].mux.Unlock()
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.cooldown"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.cooldown"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.cooldown"])
				close(proxies.Deployments[deploymentMetaNew.Name].podWatchdogStopper)
				proxies.Deployments[deploymentMetaNew.Name].mux.Lock()
				proxies.Deployments[deploymentMetaNew.Name].podWatchdogStopper = make(chan struct{})
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.pods.cooldown annotation malformed: " + err.Error())
					proxies.Deployments[deploymentMetaNew.Name].cooldown = 10
				} else {
					proxies.Deployments[deploymentMetaNew.Name].cooldown, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.pods.cooldown"])
				}
				log.Println("k8s: ", deploymentMetaNew.Name, " pod.cooldown: ", proxies.Deployments[deploymentMetaNew.Name].cooldown)
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
func (proxy *ProxyForDeployment) podScaler() {
	for {
		select {
		case msg := <-proxy.podScalerInformer:
			if msg == "new ticket" || msg == "update" {
				//check ressources
				proxy.mux.Lock()
				if proxy.Serverlist.GetAvailableTickets() < proxy.spareTickets {
					pods, err := proxy.Clientset.CoreV1().Pods(proxy.namespace).List(
						metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app.name=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
					if err != nil {
						panic(err.Error())
					}
					if len(pods.Items) < proxy.maxPods {
						mypod := v1.Pod{
							ObjectMeta: proxy.podSpec.ObjectMeta,
							Spec:       proxy.podSpec.Spec,
						}
						mypod.ObjectMeta.Labels["ipb-halle.de/k8sTicket.scaled"] = "true"
						mypod.GenerateName = strings.ToLower(proxy.Serverlist.Prefix + "-k8sticket-autoscaled-")
						_, err := proxy.Clientset.CoreV1().Pods(proxy.namespace).Create(&mypod)
						if err != nil {
							panic(err)
						}
						log.Println("k8s: podScaler: Pod created successfully")
					}
				}
				proxy.mux.Unlock()
			}
		case <-proxy.podScalerStopper:
			return
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
			pods, err := proxy.Clientset.CoreV1().Pods(proxy.namespace).List(
				metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app.name=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
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
									err := proxy.Clientset.CoreV1().Pods(proxy.namespace).Delete(pod.Name, &metav1.DeleteOptions{})
									if err != nil {
										log.Println("k8s: podWatchdog: Error deleting "+pod.Name+": ", err)
									}
								}
							}
						}
					} else {
						log.Println("k8s: podWatchdog: There is an unused pod which is not in the serverlist " + pod.Name)
						err := proxy.Clientset.CoreV1().Pods(proxy.namespace).Delete(pod.Name, &metav1.DeleteOptions{})
						if err != nil {
							log.Println("k8s: podWatchdog: Error deleting "+pod.Name+": ", err)
						}
					}
				}
			}
			proxy.mux.Unlock()
		case <-proxy.podWatchdogStopper:
			log.Println("k8s: podWatchdog: Exiting")
			//log.Println("k8s: podWatchdog: Start cleaning")
			//The following part can remove autoscaled pods when the ProxyForDeployment is deleted.
			//But this will kill all connections on the running pods, therefore it is disabled.
			/*pods, err := proxy.Clientset.CoreV1().Pods(proxy.namespace).List(metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
			if err != nil {
				panic("k8s: podWatchdog: " + err.Error())
			}
			proxy.mux.Lock()
			for _, pod := range pods.Items {
				err := proxy.Clientset.CoreV1().Pods(proxy.namespace).Delete(pod.Name, &metav1.DeleteOptions{})
				if err != nil {
					log.Println("k8s: podWatchdog: Error deleting "+pod.Name+": ", err)
				}
			}
			proxy.mux.Unlock()*/
			return
		}
	}
}
