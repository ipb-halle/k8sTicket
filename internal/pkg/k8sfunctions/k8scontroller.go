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

//ProxyForDeployment This struct includes everything necessary for running
// the ticket proxy for one deployment.
type ProxyForDeployment struct {
	Pod_controller     *Controller
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

// Controller This struct includes all components of the Controller
type Controller struct {
	Clientset interface{} //*kubernetes.Clientset
	Factory   interface{} //informers.SharedInformerFactory
	//We need interfaces here because metadatainformer and infomers
	//are still in different libraries providing different methods.
	//Maybe the implementation will change in the future to a common lib
	Informer cache.SharedInformer
	Stopper  chan struct{}
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
	proxy.Pod_controller = NewPodController(clienset, ns, prefix)
	proxy.Pod_controller.Informer.AddEventHandler(NewPodHandlerForServerlist(proxy.Serverlist, maxTickets))
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
	//defer close(proxy.Pod_controller.Stopper)
	//defer runtime.HandleCrash()
	go proxy.Pod_controller.Informer.Run(proxy.Pod_controller.Stopper)
	go proxy.Serverlist.TicketWatchdog()
	go proxy.podScaler(proxy.Serverlist.AddInformerChannel())
	go proxy.podWatchdog()
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix+"/{s}/{serverpath:.*}", proxy.Serverlist.MainHandler)
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix, proxy.Serverlist.ServeHome)
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix+"/", proxy.Serverlist.ServeHome)
	proxy.Router.HandleFunc("/"+proxy.Serverlist.Prefix+"/ws", proxy.Serverlist.ServeWs)
	go log.Fatal(proxy.Server.ListenAndServe())
}

//Stop This method stops a proxy including the http server and all running
// routines.
func (proxy *ProxyForDeployment) Stop() {
	close(proxy.Pod_controller.Stopper)
	runtime.HandleCrash()
	go func() {
		if err := proxy.Server.Shutdown(context.Background()); err != nil {
			// Error from closing listeners, or context timeout:
			log.Printf("HTTP server Shutdown: %v", err)
		}
		close(proxy.Stopper)
	}()
	<-proxy.Stopper
	for _, channel := range proxy.Serverlist.Informers {
		close(channel)
	}
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
			} else if pod_old.Status.PodIP != pod_new.Status.PodIP { //I guess this should not happen
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

//NewDeploymentHandlerForK8sconfig This function creates a new deployment handler.
// It will watch for deployments in k8s with the desired annotations and
// create (delete) the corresponding proxy. It is possible to have more than one
// deployment in a namespace, but they should have different app annotations.
func NewDeploymentHandlerForK8sconfig(c interface{}, ns string, proxies map[string]*ProxyForDeployment) cache.ResourceEventHandlerFuncs {
	clientset := c.(*kubernetes.Clientset)
	addfunction := func(obj interface{}) {
		deployment := obj.(*appsv1.Deployment)
		log.Println("k8s: Adding deployment" + deployment.Name)
		if _, ok := proxies[deployment.Name]; !ok {
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
			proxies[deployment.Name] = NewProxyForDeployment(clientset, prefix, ns, port, maxTickets, spareTickets, maxPods, cooldown, deployment.Spec.Template)
			proxies[deployment.Name].Start()
		} else {
			log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deployment.Name + " already exists!")
		}
	}
	deletionfunction := func(obj interface{}) {
		deployment := obj.(*appsv1.Deployment)
		log.Println("k8s: Deleting deployment" + deployment.Name)
		if _, ok := proxies[deployment.Name]; !ok {
			log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deployment.Name + " is not known!")
		} else {
			proxies[deployment.Name].Stop()
			delete(proxies, deployment.Name)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    addfunction,
		DeleteFunc: deletionfunction,
		UpdateFunc: func(oldObj, newObj interface{}) {
			deployment_old := oldObj.(*appsv1.Deployment)
			deployment_new := newObj.(*appsv1.Deployment)
			log.Println("k8s: NewDeploymentHandlerForK8sconfig: Deployment " + deployment_old.Name + " is updated!")
			if deployment_new.Name != deployment_old.Name {
				deletionfunction(deployment_old)
				addfunction(deployment_new)
			}
			if deployment_new.Spec.Template.String() != deployment_old.Spec.Template.String() {
				proxies[deployment_new.Name].mux.Lock()
				proxies[deployment_new.Name].PodSpec = deployment_new.Spec.Template
				proxies[deployment_new.Name].mux.Unlock()
			}
			//other changes are handled by k8s itself
			//e.g. change of pod template
		},
	}
}

//NewDeploymentHandlerForK8sconfig This function creates a new deployment handler.
// It will watch for deployments in k8s with the desired annotations and
// create (delete) the corresponding proxy. It is possible to have more than one
// deployment in a namespace, but they should have different app annotations.
func NewMetaDeploymentHandlerForK8sconfig(c interface{}, ns string, proxies map[string]*ProxyForDeployment) cache.ResourceEventHandlerFuncs {
	clientset := c.(*kubernetes.Clientset)
	return cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			deploymentPMetaOld := oldObj.(*metav1.PartialObjectMetadata)
			deploymentPMetaNew := newObj.(*metav1.PartialObjectMetadata)
			deploymentMetaOld := deploymentPMetaOld.ObjectMeta
			deploymentMetaNew := deploymentPMetaNew.ObjectMeta
			ok := true
			log.Println("k8s: NewMetaDeploymentHandlerForK8sconfig: Deployment " + deploymentMetaOld.Name + " is updated!")
			if deploymentMetaOld.Name != deploymentMetaNew.Name {
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
				if _, ok := proxies[deploymentMetaOld.Name]; !ok {
					log.Println("k8s: NewMetaDeploymentHandlerForK8sconfig: Deployment " + deploymentMetaOld.Name + " is not known!")
				} else {
					proxies[deploymentMetaOld.Name].Stop()
					dpl := proxies[deploymentMetaOld.Name]
					delete(proxies, deploymentMetaOld.Name)
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
					dpl.Port = port
					dpl.Serverlist = proxyfunctions.NewServerlist(prefix)
					dpl.Pod_controller = NewPodController(clientset, ns, prefix)
					var maxTickets int
					_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
					if err != nil {
						log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.maxTickets annotation malformed: " + err.Error())
						maxTickets = 1
					} else {
						maxTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
					}
					dpl.Pod_controller.Informer.AddEventHandler(NewPodHandlerForServerlist(dpl.Serverlist, maxTickets))
					proxies[deploymentMetaNew.Name] = dpl
					proxies[deploymentMetaNew.Name].Start()
				}
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
				proxies[deploymentMetaNew.Name].mux.Lock()
				var maxTickets int
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.maxTickets annotation malformed: " + err.Error())
					maxTickets = 1
				} else {
					maxTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"])
				}
				proxies[deploymentMetaNew.Name].Serverlist.ChangeAllMaxTickets(maxTickets)
				proxies[deploymentMetaNew.Name].mux.Unlock()
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"])
				proxies[deploymentMetaNew.Name].mux.Lock()
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.spareTickets annotation malformed: " + err.Error())
					proxies[deploymentMetaNew.Name].spareTickets = 2
				} else {
					proxies[deploymentMetaNew.Name].spareTickets, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.spareTickets"])
				}
				proxies[deploymentMetaNew.Name].mux.Unlock()
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"])
				proxies[deploymentMetaNew.Name].mux.Lock()
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.maxPods annotation malformed: " + err.Error())
					proxies[deploymentMetaNew.Name].maxPods = 1
				} else {
					proxies[deploymentMetaNew.Name].maxPods, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxPods"])
				}
				proxies[deploymentMetaNew.Name].mux.Unlock()
			}
			if deploymentMetaOld.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"] != deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"] {
				_, err := strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"])
				close(proxies[deploymentMetaNew.Name].podWatchdogStopper)
				proxies[deploymentMetaNew.Name].mux.Lock()
				proxies[deploymentMetaNew.Name].podWatchdogStopper = make(chan struct{})
				if err != nil {
					log.Println("k8s: Deployment: " + deploymentMetaNew.Name + "ipb-halle.de/k8sticket.deployment.Podcooldown annotation malformed: " + err.Error())
					proxies[deploymentMetaNew.Name].cooldown = 10
				} else {
					proxies[deploymentMetaNew.Name].cooldown, _ = strconv.Atoi(deploymentMetaNew.GetAnnotations()["ipb-halle.de/k8sticket.deployment.Podcooldown"])
				}
				proxies[deploymentMetaNew.Name].mux.Unlock()
				go proxies[deploymentMetaNew.Name].podWatchdog()
			}
			//other changes are handled by k8s itself
			//e.g. change of pod template
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
			return
		}
	}
}
