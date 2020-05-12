package k8sfunctions

import (
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/culpinnis/k8sTicket/internal/pkg/proxyfunctions"
	"k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
	//"k8s.io/apimachinery/pkg/api/errors"
	//
	// Uncomment to load all auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/openstack"
)

const (
	default_port = 80
	default_path = "/"
)

func New_handler_for_serverlist(list *proxyfunctions.Serverlist, maxtickets int) cache.ResourceEventHandlerFuncs {
	addfunction := func(obj interface{}) {
		pod := obj.(*v1.Pod)
		log.Println("k8s: New Pod " + pod.Name)
		if pod.Status.Phase == v1.PodRunning {
			conf, err := PodToConfig(pod)
			if err == nil {
				err := list.AddServer(pod.Name, maxtickets, conf)
				if err != nil {
					log.Println("k8s: AddServer:  ", err)
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
			} //I think all other changes will not affect the proxy service
		},
	})
}

// Namespace This function returns the namespace of the running pod
func Namespace() string {
	// This way assumes you've set the POD_NAMESPACE environment variable using the downward API.
	// This check has to be done first for backwards compatibility with the way InClusterConfig was originally set up
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		return ns
	}

	// Fall back to the namespace associated with the service account token, if available
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}

	return "default"
}

// PodToConfig This function reads the k8sTicket annotations and creates
// a config for the reverse proxy.
func PodToConfig(pod *v1.Pod) (proxyfunctions.Config, error) {
	ip := pod.Status.PodIP
	if ip == "" {
		return proxyfunctions.Config{}, errors.New("Pod has no vaild IP")
	}
	cpath := default_path
	cport := default_port
	if path, ok := pod.GetAnnotations()["ipb-halle.de/k8sTicket.path"]; ok {
		cpath = path
	}
	if port, ok := pod.GetAnnotations()["ipb-halle.de/k8sTicket.port"]; ok {
		_, err := strconv.Atoi(port)
		if err != nil {
			log.Println("k8s: Annotation: ", pod.GetName(), ": ", port, ": ", err, " Default ", default_port, "will be used")
			cport = default_port
		} else {
			cport, _ = strconv.Atoi(port)
		}
	}
	return proxyfunctions.Config{Host: ip + ":" + strconv.Itoa(cport), Path: cpath}, nil
}
