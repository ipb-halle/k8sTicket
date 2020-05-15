package main

import (
	"runtime"

	"github.com/culpinnis/k8sTicket/internal/pkg/k8sfunctions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	namespace := k8sfunctions.Namespace()
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
	proxymap := make(map[string]*k8sfunctions.Proxy_for_deployment)
	deploymentController := k8sfunctions.New_deployment_controller(namespace)
	deploymentController.Informer.AddEventHandler(k8sfunctions.New_deployment_handler_for_k8sconfig(clientset, namespace, proxymap))
	deploymentController.Informer.Run(deploymentController.Stopper)
	runtime.Goexit()
}
