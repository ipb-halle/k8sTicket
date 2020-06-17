package main

import (
	"runtime"

	"github.com/culpinnis/k8sTicket/internal/pkg/k8sfunctions"
)

func main() {
	namespace := k8sfunctions.Namespace()

	proxymap := make(map[string]*k8sfunctions.ProxyForDeployment)
	deploymentController := k8sfunctions.NewDeploymentController(namespace)
	deploymentMetaController := k8sfunctions.NewDeploymentMetaController(namespace)

	deploymentController.Informer.AddEventHandler(k8sfunctions.NewDeploymentHandlerForK8sconfig(deploymentController.Clientset, namespace, proxymap))
	go deploymentController.Informer.Run(deploymentController.Stopper)

	//Using the clientset of deploymentController is not a mistake, its done on purpose because it will be used by the PodController!
	deploymentMetaController.Informer.AddEventHandler(k8sfunctions.NewMetaDeploymentHandlerForK8sconfig(deploymentController.Clientset, namespace, proxymap))
	go deploymentMetaController.Informer.Run(deploymentMetaController.Stopper)

	runtime.Goexit()
}
