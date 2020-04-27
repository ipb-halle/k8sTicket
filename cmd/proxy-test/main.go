package main

import (
	"log"
	"net/http"
	"time"

	"github.com/culpinnis/k8sTicket/internal/pkg/proxyfunctions"
	"github.com/gorilla/mux"
)

func main() {
	r := mux.NewRouter()
	list := new(proxyfunctions.Serverlist)
	var prefix = "gmweb"
	list.Prefix = prefix

	if _, err := list.AddServer(1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}); err != nil {
		log.Println("Error Occured: ", err)
	}
	if _, err := list.AddServer(1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}); err != nil {
		log.Println("Error Occured: ", err)
	}
	if _, err := list.AddServer(1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}); err != nil {
		log.Println("Error Occured: ", err)
	}
	go list.TicketWatchdog()
	//no need to check the error in the test application
	////nolint:errcheck
	go time.AfterFunc(20*time.Second, func() { list.SetServerDeletion(1) })
	//nolint:errcheck
	go time.AfterFunc(60*time.Second, func() { list.AddServer(1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}) })
	// go time.AfterFunc(90*time.Second, func() { list.AddServer(1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}) })
	r.HandleFunc("/"+list.Prefix+"/{s}/{serverpath:.*}", list.MainHandler)
	r.HandleFunc("/"+list.Prefix, list.ServeHome)
	r.HandleFunc("/"+list.Prefix+"/", list.ServeHome)
	r.HandleFunc("/"+list.Prefix+"/ws", list.ServeWs)
	log.Fatal(http.ListenAndServe(":9001", r))
}
