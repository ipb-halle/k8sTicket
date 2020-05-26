package k8sfunctions

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/culpinnis/k8sTicket/internal/pkg/proxyfunctions"
	gorilla "github.com/gorilla/mux"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/informers/internalinterfaces"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
)

type Proxy_for_deployment struct {
	Pod_controller *Controller
	Clientset      *kubernetes.Clientset
	Serverlist     *proxyfunctions.Serverlist
	Server         *http.Server
	Router         *gorilla.Router
	Namespace      string
	Port           string
	PodSpec        v1.PodTemplateSpec
	Stopper        chan struct{}
	spareTickets   int
	maxPods        int
}

func New_Proxy_for_deployment(clienset *kubernetes.Clientset, prefix string, ns string, port string, maxTickets int, spareTickets int, maxPods int, podspec v1.PodTemplateSpec) *Proxy_for_deployment {
	proxy := Proxy_for_deployment{}
	router := gorilla.NewRouter()
	proxy.Serverlist = proxyfunctions.NewServerlist(prefix)
	proxy.Namespace = ns
	proxy.Port = port
	proxy.Pod_controller = New_pod_controller(clienset, ns, prefix)
	proxy.Pod_controller.Informer.AddEventHandler(New_pod_handler_for_serverlist(proxy.Serverlist, maxTickets))
	proxy.Clientset = clienset
	proxy.PodSpec = podspec
	proxy.Stopper = make(chan struct{})
	proxy.Router = router
	proxy.Server = &http.Server{Addr: ":" + port, Handler: router}
	proxy.spareTickets = spareTickets
	proxy.maxPods = maxPods
	return &proxy
}

func (proxy *Proxy_for_deployment) Start() {
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

func (proxy *Proxy_for_deployment) Stop() {
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
}

// Controller This struct includes a components of the Controller
type Controller struct {
	clientset *kubernetes.Clientset
	factory   informers.SharedInformerFactory
	Informer  cache.SharedInformer
	Stopper   chan struct{}
}

// New_pod_controller This function makes a new controller for our proxy
// with a InClusterConfig and a given namespace to watch.
func New_pod_controller(clientset *kubernetes.Clientset, ns string, app string) *Controller {
	factory := informers.NewSharedInformerFactoryWithOptions(clientset,
		0,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(internalinterfaces.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "ipb-halle.de/k8sticket.deployment.app=" + app
		})))
	informer := factory.Core().V1().Pods().Informer()
	stopper := make(chan struct{})
	return (&Controller{
		clientset: clientset,
		factory:   factory,
		Informer:  informer,
		Stopper:   stopper,
	})
}

// New_pod_template_controller This function makes a new controller for our proxy
// with a InClusterConfig and a given namespace to watch.
func New_deployment_controller(ns string) Controller {
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
		0,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(internalinterfaces.TweakListOptionsFunc(func(options *metav1.ListOptions) {
			options.LabelSelector = "k8sTicket=true"
		})))
	informer := factory.Apps().V1().Deployments().Informer()
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
func New_pod_handler_for_serverlist(list *proxyfunctions.Serverlist, maxtickets int) cache.ResourceEventHandlerFuncs {
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

func New_deployment_handler_for_k8sconfig(clientset *kubernetes.Clientset, ns string, proxies map[string]*Proxy_for_deployment) cache.ResourceEventHandlerFuncs {
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
			log.Println("k8s: Adding deployment " + deployment.Name + " parameters: ")
			log.Println("k8s: Port: " + port)
			log.Println("k8s: app: " + prefix)
			log.Println("k8s: maxTickets: " + strconv.Itoa(maxTickets))
			proxies[deployment.Name] = New_Proxy_for_deployment(clientset, prefix, ns, port, maxTickets, spareTickets, maxPods, deployment.Spec.Template)
			go proxies[deployment.Name].Start()
			//go proxies[deployment.Name].podWatchdog()
		} else {
			log.Println("k8s: New_deployment_handler_for_k8sconfig: Deployment " + deployment.Name + " already exists!")
		}
		/*podtmp := deployment.Spec.Template
		mypod := v1.Pod{
			ObjectMeta: podtmp.ObjectMeta,
			Spec:       podtmp.Spec,
		}
		mypod.ObjectMeta.Labels["k8sautogen"] = "true"
		mypod.GenerateName = "testpod"
		_, err := clientset.CoreV1().Pods(ns).Create(&mypod)
		if err != nil {
			panic(err)
		}
		fmt.Println("Pod created successfully...")*/
	}
	deletionfunction := func(obj interface{}) {
		deployment := obj.(*appsv1.Deployment)
		log.Println("k8s: Deleting deployment" + deployment.Name)
		if _, ok := proxies[deployment.Name]; !ok {
			log.Println("k8s: New_deployment_handler_for_k8sconfig: Deployment " + deployment.Name + " is not known!")
		} else {
			proxies[deployment.Name].Stop()
			//Deletion?
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    addfunction,
		DeleteFunc: deletionfunction,
		UpdateFunc: func(oldObj, newObj interface{}) {
			ok := true
			deployment_old := oldObj.(*appsv1.Deployment)
			deployment_new := newObj.(*appsv1.Deployment)
			if deployment_old.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"] != deployment_new.GetAnnotations()["ipb-halle.de/k8sticket.deployment.port"] {
				ok = false
			}
			if deployment_old.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"] != deployment_new.GetAnnotations()["ipb-halle.de/k8sticket.deployment.app"] {
				ok = false
			}
			if deployment_old.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"] != deployment_new.GetAnnotations()["ipb-halle.de/k8sticket.deployment.maxTickets"] {
				ok = false
			}
			if !ok {
				proxies[deployment_old.Name].Stop()
				go proxies[deployment_new.Name].Start()
			}
			//other changes are handled by k8s itself
			//e.g. change of pod template
		},
	}
}

func (proxy *Proxy_for_deployment) podScaler(informer chan string) {
	for msg := range informer {
		if msg == "new ticket" {
			//check ressources
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
		}
	}
}

func (proxy *Proxy_for_deployment) podWatchdog() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	//defer list.mux.Unlock()
	for {
		<-ticker.C
		log.Println("k8s: podWatchdog: Start cleaning")
		pods, err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).List(metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
		if err != nil {
			panic(err.Error()) //Make a better error message!
		}
		if proxy.Serverlist.GetAvailableTickets() > proxy.spareTickets {
			for _, pod := range pods.Items {
				if _, ok := proxy.Serverlist.Servers[pod.Name]; ok {
					log.Println("k8s: podWatchdog: Checking "+pod.Name+" with ", len(proxy.Serverlist.Servers[pod.Name].Tickets), " Tickets")
					if len(proxy.Serverlist.Servers[pod.Name].Tickets) == 0 {
						if time.Since(proxy.Serverlist.Servers[pod.Name].LastUsed).Milliseconds() > int64(30)*time.Second.Milliseconds() { //Change me to annotation
							if (proxy.Serverlist.GetAvailableTickets() - proxy.Serverlist.Servers[pod.Name].MaxTickets) >= proxy.spareTickets {
								//err := proxy.Serverlist.SetServerDeletion(pod.Name)
								//if err != nil {
								//log.Println("k8s: SetServerDeletion:  ", err)
								//} else {
								err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).Delete(pod.Name, &metav1.DeleteOptions{})
								if err != nil {
									log.Println("k8s: podWatchdog: Error deleting "+pod.Name+": ", err)
									//}
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
	}
}
