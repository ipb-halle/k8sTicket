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
	var prefix = "gmweb"
	list := proxyfunctions.NewServerlist(prefix)

	if err := list.AddServer("one", 1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}); err != nil {
		log.Println("Error Occured: ", err)
	}
	if err := list.AddServer("two", 1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}); err != nil {
		log.Println("Error Occured: ", err)
	}
	if err := list.AddServer("three", 1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}); err != nil {
		log.Println("Error Occured: ", err)
	}
	go list.TicketWatchdog()
	//no need to check the error in the test application
	////nolint:errcheck
	go time.AfterFunc(20*time.Second, func() { list.SetServerDeletion("one") })
	//nolint:errcheck
	go time.AfterFunc(60*time.Second, func() { list.AddServer("four", 1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}) })
	// go time.AfterFunc(90*time.Second, func() { list.AddServer(1, proxyfunctions.Config{Path: "/", Host: "127.0.0.1:3838"}) })
	r.HandleFunc("/"+list.Prefix+"/{s}/{serverpath:.*}", list.MainHandler)
	r.HandleFunc("/"+list.Prefix, list.ServeHome)
	r.HandleFunc("/"+list.Prefix+"/", list.ServeHome)
	r.HandleFunc("/"+list.Prefix+"/ws", list.ServeWs)
	log.Fatal(http.ListenAndServe(":9001", r))
}
