/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Note: the example only works with the code within the same release/branch.
package main

import (
	"fmt"
	"log"
	"net/http"
	//"k8s.io/apimachinery/pkg/api/errors"

	"github.com/culpinnis/k8sTicket/internal/pkg/k8sfunctions"
	"github.com/culpinnis/k8sTicket/internal/pkg/proxyfunctions"
	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest" //
	// Uncomment to load all auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/openstack"
)

func main() {
	//http testing
	r := mux.NewRouter()
	var prefix = "test"
	list := proxyfunctions.NewServerlist(prefix)

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
	myns := k8sfunctions.Namespace()

	//test controller Functions
	mycontroller := k8sfunctions.New_proxy_controller(myns)
	defer close(mycontroller.Stopper)
	defer runtime.HandleCrash()
	myhandler := k8sfunctions.New_handler_for_serverlist(list, 1)
	mycontroller.Informer.AddEventHandler(myhandler)
	go mycontroller.Informer.Run(mycontroller.Stopper)
	fmt.Printf("********\nI am running in %s\n********\n", myns)
	// get pods in all the namespaces by omitting namespace
	// Or specify namespace to get pods in particular namespace
	pods, err := clientset.CoreV1().Pods(myns).List(metav1.ListOptions{})
	if err != nil {
		panic(err.Error())
	}
	fmt.Printf("\nThere are %d pods in the namespace\n", len(pods.Items))
	/*
		for _, pod := range pods.Items {
			fmt.Printf("Found Pod %s, IP: %s", pod.GetName(), pod.Status.PodIP)
			fmt.Print("\nFound Labels\n")
			for label, value := range pod.GetLabels() {
				fmt.Printf("########%s : %s\n", label, value)
			}
			fmt.Print("\nFound Annotations\n")
			for annotation, value := range pod.GetAnnotations() {
				fmt.Printf("########%s : %s\n", annotation, value)
			}
			fmt.Print("\n\n")
			//testing part
			serverconfig, err := k8sfunctions.PodToConfig(pod)
			if err == nil {
				if _, err := list.AddServer(1, serverconfig); err != nil {
					log.Println("Error Occured: ", err)
				}
			}
		} */
	go list.TicketWatchdog()
	r.HandleFunc("/"+list.Prefix+"/{s}/{serverpath:.*}", list.MainHandler)
	r.HandleFunc("/"+list.Prefix, list.ServeHome)
	r.HandleFunc("/"+list.Prefix+"/", list.ServeHome)
	r.HandleFunc("/"+list.Prefix+"/ws", list.ServeWs)
	log.Fatal(http.ListenAndServe(":9001", r))
}
