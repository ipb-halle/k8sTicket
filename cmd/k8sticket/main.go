package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/culpinnis/k8sTicket/internal/pkg/k8sfunctions"
)

func main() {
	log.Println("main: Starting!")

	namespace := k8sfunctions.Namespace()

	proxymap := make(map[string]*k8sfunctions.ProxyForDeployment)
	deploymentController := k8sfunctions.NewDeploymentController(namespace)
	deploymentMetaController := k8sfunctions.NewDeploymentMetaController(namespace)

	deploymentController.Informer.AddEventHandler(k8sfunctions.NewDeploymentHandlerForK8sconfig(deploymentController.Clientset, namespace, proxymap))
	go deploymentController.Informer.Run(deploymentController.Stopper)

	//Using the clientset of deploymentController is not a mistake, its done on purpose because it will be used by the PodController!
	deploymentMetaController.Informer.AddEventHandler(k8sfunctions.NewMetaDeploymentHandlerForK8sconfig(deploymentController.Clientset, namespace, proxymap))
	go deploymentMetaController.Informer.Run(deploymentMetaController.Stopper)
	//Let the subroutines do their job until we receive a exit message from the OS

	exitSignal := make(chan os.Signal, 1)
	signal.Notify(exitSignal, syscall.SIGINT, syscall.SIGTERM)
	<-exitSignal
	log.Println("main: Exiting!")
	close(deploymentController.Stopper)
	log.Println("main: DeploymentController stopped!")
	close(deploymentMetaController.Stopper)
	log.Println("main: DeploymentMetaController stopped!")
	for _, proxy := range proxymap {
		proxy.Stop()
	}
	log.Println("Bye!")
}
