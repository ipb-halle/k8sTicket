package proxyfunctions

//General remarks
//For all things that need a mux, it would be much better to write
//getter and setter functions instead of telling other developers to
//lock and unlock the mux manually.
//Besides, it would be more intelligent to allow write access only for
//certain objects

import (
	"container/list"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 10 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	//pingPeriod = (pongWait * 9) / 10
	pingPeriod = 5 * time.Second

	//the time during ticket updates and ticket removal
	ticketTime = 3 * time.Second
)

//Stucts for Server and Tickets

//Config The Config has a Path and a Host.
type Config struct {
	Path string
	Host string
}

//ticket A ticket has redundant information server and token for easier access.
// It will be updated everytime it is used.
// Whenever something is modified, lock the Mux before!

type ticket struct {
	LastUsed time.Time
	server   *server
	token    string
	Mux      sync.Mutex
}

//server A server knows its maximal number of tickets, its Config,
// all tickets based on their tockens and it includes the http.Handler
// serving the proxy requests.
// Whenever something is modified, lock the Mux before!
type server struct {
	maxTickets int
	Config     Config
	Tickets    map[string]*ticket
	Handler    http.Handler
	UseAllowed bool
	Mux        sync.Mutex
	LastUsed   time.Time
	Name       string
}

//Serverlist The Serverlist includes the servers in a slice and the asked quiers (Tqueries).
// All new connections will lead to new Tqueries. When there are free resources
// available, a ticket will be generated and the Tqueries will be removed from
// the list.
type Serverlist struct {
	Servers   map[string]*server
	Tqueries  list.List
	Prefix    string
	Mux       sync.Mutex
	Informers []chan string //maybe use a list.List if deletion of channels gets important
}

//NewServerlist Creates a new Serverlist, needs a prefix (app label).
func NewServerlist(prefix string) *Serverlist {
	list := new(Serverlist)
	list.Servers = make(map[string]*server)
	list.Prefix = prefix
	return (list)
}

// This function generates a token like 31f4ef3d.
// It is used to identify the tickets in k8sticket.
func tokenGenerator() string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	if err != nil {
		log.Println("FATAL: tokenGenerator: ", err)
		os.Exit(1)
	}
	return fmt.Sprintf("%x", b)
}

//Serverlist methods

//ChangeMaxTickets This function changes the MaxTickets on all servers
func (list *Serverlist) ChangeAllMaxTickets(newMaxTickets int) {
	for name := range list.Servers {
		list.Servers[name].ChangeMaxTickets(newMaxTickets)
	}
}

// AddServer This function adds a new server to the server list.
// It requieres a name, the maximal number of tickets that can be
// handeled by this server and the Config
func (list *Serverlist) AddServer(name string, maxtickets int, Config Config) error {
	//defer list.Mux.Unlock()
	list.deletionmanager() //first check if servers should be deleted
	list.Mux.Lock()
	log.Println("Server: Adding Server " + name + " " + Config.Host + Config.Path)
	if _, ok := list.Servers[name]; !ok {
		list.Servers[name] = &server{
			maxTickets: maxtickets,
			Config:     Config,
			Handler:    generateProxy(Config),
			UseAllowed: true,
			Tickets:    make(map[string]*ticket),
			Name:       name,
		}
	} else {
		list.Mux.Unlock()
		return (errors.New("Server with the name " + name + "already exists"))
	}
	list.Mux.Unlock()
	list.querrymanager()
	return nil
}

// SetServerDeletion This function marks a server to be deleted.
func (list *Serverlist) SetServerDeletion(name string) error {
	list.Mux.Lock()
	if _, ok := list.Servers[name]; !ok {
		list.Mux.Unlock()
		return (errors.New("Server deletion: " + name + "does not exist"))
	}
	list.Mux.Unlock()
	list.Servers[name].Mux.Lock()
	list.Servers[name].UseAllowed = false
	list.Servers[name].Mux.Unlock()
	list.deletionmanager()
	return nil
}

// RemoveServer This function tries to delete a server. It will only succeed if the server
// is not occupied by a ticket!
// If the server is still busy, it will be marked for deletion by the
// UseAllowed bool.
func (list *Serverlist) removeServer(name string) error {
	if _, ok := list.Servers[name]; !ok {
		return (errors.New("Server deletion: " + name + " does not exist"))
	}
	//Check if there are still open sessions
	if len(list.Servers[name].Tickets) == 0 {
		log.Println("Server: Deleting server " + name)
		delete(list.Servers, name)
	} else {
		log.Println("Server: Server " + name + " is marked for deletion, but occupied.")
		return (errors.New("server deletion: server still occupied"))
	}
	return (nil)
}

//deletionmanager This function tries to delete servers that are marked for
// deletion. This is necessary because we do not want to delete a server
// that is used. If used in k8s the pod deletion of a used server
// will lead to a connection interuption anyway resulting in ticket deletion.
func (list *Serverlist) deletionmanager() {
	defer list.Mux.Unlock()
	list.Mux.Lock()
	for name := range list.Servers {
		list.Servers[name].Mux.Lock()
		if !list.Servers[name].UseAllowed {
			list.Servers[name].Mux.Unlock() //unlock the server before it is removed.
			//we won't check the error of removeServer anymore since the serverid was
			//changed to string instead of int
			//nolint:errcheck
			list.removeServer(name)
		} else { //needs to be with an else, because if the server was removed, we can not unlock the Mux anymore
			list.Servers[name].Mux.Unlock()
		}
	}
}

//GetAvailableTickets This function returns the number of all available
//tickets on all known and active servers.
func (list *Serverlist) GetAvailableTickets() int {
	out := 0
	for name := range list.Servers {
		list.Servers[name].Mux.Lock()
		if list.Servers[name].UseAllowed {
			out = out + list.Servers[name].maxTickets - len(list.Servers[name].Tickets)
		}
		list.Servers[name].Mux.Unlock()
	}
	return out
}

// addTicket This functions adds a new ticket to the Serverlist on the first
// available server. It will return an error if there are no free Tickets
// availabe in the Serverlist.
func (list *Serverlist) addTicket() (*ticket, error) {
	for name := range list.Servers {
		list.Servers[name].Mux.Lock()
		log.Println("Ticket: Trying " + name)
		if list.Servers[name].hasSlots() && list.Servers[name].UseAllowed {
			list.Servers[name].Mux.Unlock()
			return list.Servers[name].newTicket(), nil
		}
		list.Servers[name].Mux.Unlock()
	}
	return nil, errors.New("No ticket left")
}

func (list *Serverlist) AddInformerChannel() chan string {
	chanInformer := make(chan string, 1)
	list.Mux.Lock()
	list.Informers = append(list.Informers, chanInformer)
	list.Mux.Unlock()
	return chanInformer
}

// TicketWatchdog This function checks if tickets a still valid (updated in specified time).
// If the ticket was not updated in time, it will be removed from the server.
func (list *Serverlist) TicketWatchdog() {
	ticker := time.NewTicker(ticketTime)
	defer ticker.Stop()
	//defer list.Mux.Unlock()
	for {
		<-ticker.C
		list.Mux.Lock()
		for id := range list.Servers {
			for token := range list.Servers[id].Tickets {
				list.Servers[id].Tickets[token].Mux.Lock()
				if time.Since(list.Servers[id].Tickets[token].LastUsed).Milliseconds() > ticketTime.Milliseconds() {
					list.Servers[id].Tickets[token].Mux.Unlock()
					delete(list.Servers[id].Tickets, token)
					log.Println("Ticket: Deleting ticket " + token)
					for _, channel := range list.Informers {
						channel <- "delete ticket"
					}
				} else {
					list.Servers[id].Tickets[token].Mux.Unlock()
				}
			}
		}
		list.Mux.Unlock()
		list.deletionmanager()
		list.querrymanager()
	}
}

// querrymanager This function checks the Tqueries and creates a new ticket if resources are
// available.
func (list *Serverlist) querrymanager() {
	defer list.Mux.Unlock()
	list.Mux.Lock()
	if list.Tqueries.Len() > 0 {
		log.Println("Queries: There are " + strconv.Itoa(list.Tqueries.Len()) + " waiting")
		t, err := list.addTicket()
		if err == nil {
			ChannelElement := list.Tqueries.Front()
			ChannelValue := ChannelElement.Value
			channel, ok := ChannelValue.(chan *ticket)
			if !ok {
				log.Printf("FATAL: got data of type %T but wanted chan!", ChannelValue)
				os.Exit(1)
			}
			channel <- t
			close(channel)
			list.Tqueries.Remove(ChannelElement)
			for _, chann := range list.Informers {
				chann <- "new ticket"
			}
		} else {
			log.Println("Serverlist: querrymanager: ", err)
		}
	}
}

//Server methods

//ChangeMaxTickets Changes the maxTickets value of a server taking the servers mutex into account.
func (server *server) ChangeMaxTickets(newMaxTickets int) {
	server.Mux.Lock()
	server.maxTickets = newMaxTickets
	server.Mux.Unlock()
}

//GetMaxTickets Returns the maxTickets value of a server taking the servers mutex into account.
func (server *server) GetMaxTickets() int {
	server.Mux.Lock()
	defer server.Mux.Unlock()
	return server.maxTickets
}

//GetLastUsed Returns the LastUsed value of a server taking the servers mutex into account.
func (server *server) GetLastUsed() time.Time {
	server.Mux.Lock()
	defer server.Mux.Unlock()
	return server.LastUsed
}

//HasTickets Returns true if the server has no tickets.
func (server *server) HasNoTickets() bool {
	server.Mux.Lock()
	defer server.Mux.Unlock()
	return (len(server.Tickets) == 0)
}

// hasSlots This function checks if a server has still free slots for new tickets.
func (server *server) hasSlots() bool {
	return len(server.Tickets) < server.maxTickets
}

// newTicket This function adds a new ticket to a server and returns the new ticket.
// It is only used internally.
func (server *server) newTicket() *ticket {
	defer server.Mux.Unlock()
	server.Mux.Lock()
	token := tokenGenerator()
	newTicket := &ticket{
		LastUsed: time.Now(),
		server:   server,
		token:    token,
	}
	server.Tickets[token] = newTicket
	return (newTicket)
}

// update This functions updates a ticket as long as the chan is open.
func (ticket *ticket) update(alive chan struct{}) {
	ticker := time.NewTicker(ticketTime - 10*time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ticket.Mux.Lock()
			curtime := time.Now()
			ticket.LastUsed = curtime
			ticket.server.Mux.Lock()
			ticket.server.LastUsed = curtime
			ticket.server.Mux.Unlock()
			log.Println("Ticket: refreshing: ", ticket.token+"  "+ticket.LastUsed.Format("2006-01-02 15:04:05"))
			ticket.Mux.Unlock()
		case <-alive:
			return
		}
	}
}

//Functions for serving the webcontent

//MainHandler This function provides the toplevel handler for the proxy requests
func (list *Serverlist) MainHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	servername := vars["s"]
	log.Println("PATH:", vars["serverpath"])
	list.callServer(w, r, servername)
}

//callServer This function redirects client requests to the according proxy handlers.
func (list *Serverlist) callServer(w http.ResponseWriter, r *http.Request, name string) {
	alive := make(chan struct{})
	defer close(alive)
	list.Mux.Lock()
	if _, ok := list.Servers[name]; ok {
		if list.Servers[name].Handler != nil {
			//Middleware check Ticket
			cookie, err := r.Cookie("stoken")
			if err == http.ErrNoCookie {
				http.Error(w, "No valid cookie!", http.StatusForbidden)
				list.Mux.Unlock()
			} else {
				//token := r.Header.Get("X-Session-Token")
				token := cookie.Value
				if ticket, ok := list.Servers[name].Tickets[token]; ok {
					list.Mux.Unlock()
					list.Servers[name].Mux.Lock()
					curtime := time.Now()
					list.Servers[name].LastUsed = curtime
					list.Servers[name].Mux.Unlock()
					ticket.Mux.Lock()
					ticket.LastUsed = curtime
					log.Println("Ticket:", token+"  "+ticket.LastUsed.Format("2006-01-02 15:04:05"))
					ticket.Mux.Unlock()
					go ticket.update(alive)
					list.Servers[name].Mux.Lock()
					ThisHandler := &list.Servers[name].Handler
					list.Servers[name].Mux.Unlock()
					http.StripPrefix("/"+list.Prefix+"/"+name+"/", *ThisHandler).ServeHTTP(w, r)
				} else {
					list.Mux.Unlock()
					http.Error(w, "Forbidden", http.StatusForbidden)
				}
			}
		} else {
			list.Mux.Unlock()
		}
	} else {
		list.Mux.Unlock()
		http.NotFound(w, r)
	}
}

//ServeHome This function serves the home page.
func (list *Serverlist) ServeHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/"+list.Prefix && r.URL.Path != "/"+list.Prefix+"/" {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	http.ServeFile(w, r, "web/static/home.html")
}

//ping This is just the ws ping
func ping(ws *websocket.Conn, done chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			log.Println("ping: Ping!")
			if err := ws.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(writeWait)); err != nil {
				log.Println("ping:", err)
				close(done)
			}
		case <-done:
			return
		}
	}
}

var upgrader = websocket.Upgrader{}

//ServeWs This handler serves the Websocket connection to acquire the cookie & ticket.
func (list *Serverlist) ServeWs(w http.ResponseWriter, r *http.Request) {
	running := make(chan struct{})
	wswrite := make(chan string)
	//defer close(running)
	log.Print("WS: connection opened!\n")
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	go func() {
		for {
			select {
			case string := <-wswrite:
				if err := ws.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
					log.Println("Ticket: WS: SetWriteDeadline: ", err)
				}
				if err := ws.WriteMessage(websocket.TextMessage, []byte(string)); err != nil {
					log.Println("Ticket: WS: WriteMessage: ", err)
					if _, ok := <-running; ok { //if the channel is still open, close it
						close(running)
					}
				}
			case <-running:
				if err := ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")); err != nil {
					log.Println("Ticket: WS: WriteMessage: CloseMessage: ", err)
				}
				ws.Close()
				return
			}
		}
	}()
	go writeHello(ws, running, wswrite)
	go ping(ws, running)
	ws.SetPongHandler(func(string) error {
		err := ws.SetReadDeadline(time.Now().Add(pongWait))
		log.Println("Pong: WS: SetReadDeadline: ", err)
		return err
	})
	querry := make(chan *ticket, 1)
	list.Mux.Lock()
	myElement := list.Tqueries.PushBack(querry)
	list.Mux.Unlock()
	defer func() {
		list.Mux.Lock()
		list.Tqueries.Remove(myElement)
		list.Mux.Unlock()
	}()
	list.querrymanager()
	go func() {
		ticket := <-querry
		wswrite <- "tkn#" + ticket.token + "@" + ticket.server.Name
		close(running)
	}()
	ticketticker := time.NewTicker(10 * time.Second)
	defer ticketticker.Stop()
	for {
		select {
		case <-ticketticker.C:
			wswrite <- "msg#Waiting for ticket, please hold the line!"
		case <-running:
			return
		}
	}
}

//writeHello Writes a message to the frontend to welcome the user
func writeHello(ws *websocket.Conn, done chan struct{}, writech chan string) {
	writech <- "msg#Welcome generating ticket!"
}

//generateProxy Generates a proxy based on a given configuration.
func generateProxy(conf Config) http.Handler {
	proxy := &httputil.ReverseProxy{Director: func(req *http.Request) {
		originHost := conf.Host
		req.Header.Add("X-Forwarded-Host", req.Host)
		req.Header.Add("X-Origin-Host", originHost)
		req.Host = originHost
		req.URL.Host = originHost
		req.URL.Scheme = "http"
		req.URL.Path = conf.Path + req.URL.Path

	}, Transport: &http.Transport{
		Dial: (&net.Dialer{
			Timeout: 5 * time.Second,
		}).Dial,
	}}

	return proxy
}
