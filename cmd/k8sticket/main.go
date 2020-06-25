package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/culpinnis/k8sTicket/pkg/k8sfunctions"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//main This is the k8sTicket application.
func main() {
	log.Println("main: Starting!")

	namespace := k8sfunctions.Namespace()

	proxymap := k8sfunctions.NewProxyMap()
	metric := k8sfunctions.NewPMetric()
	prometheus.MustRegister(metric.CurrentFreeTickets)
	prometheus.MustRegister(metric.CurrentScaledPods)
	prometheus.MustRegister(metric.CurrentUsers)
	prometheus.MustRegister(metric.TotalUsers)
	http.Handle("/metrics", promhttp.Handler())

	// Start prometheus metric
	go func() {
		err := http.ListenAndServe(":9999", nil)
		if err != nil {
			log.Println("Main: Error", err)
			os.Exit(1)
		}
	}()

	deploymentController := k8sfunctions.NewDeploymentController(namespace)
	deploymentMetaController := k8sfunctions.NewDeploymentMetaController(namespace)

	deploymentController.Informer.AddEventHandler(
		k8sfunctions.NewDeploymentHandlerForK8sconfig(deploymentController.Clientset, namespace, proxymap, &metric))
	go deploymentController.Informer.Run(deploymentController.Stopper)

	//Using the clientset of deploymentController is not a mistake,
	//it's done on purpose because it will be used by the PodController!
	deploymentMetaController.Informer.AddEventHandler(
		k8sfunctions.NewMetaDeploymentHandlerForK8sconfig(deploymentController.Clientset, namespace, proxymap, &metric))
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
	for _, proxy := range proxymap.Deployments {
		proxy.Stop()
	}
	log.Println("Bye!")
}
