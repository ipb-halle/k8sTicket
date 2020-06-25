# Documentation
## General Introduction
k8sTicket is a reverse proxy with a built-in Kubernetes controller. It is designed for a user-based deployment and scaling of applications in Kubernetes. Kubernetes offers a horizontal Pod auto-scaler (HPA) to scale applications based on different metrics (e.g. memory usage). At the moment the horizontal pod auto-scaler has the disadvantage that it is made for stateless applications. With stateful applications, there are several disadvantages using the built-in HPA:

- interactive data analysis software often leads to unpredictable and inconsistent load based on the user's calculations
	- scaling based on resources like memory or CPU usage is not effective in this situation
 	- the experience of other users utilizing the same instance of the application may suffer from the high load of other users
 	- down-scaling can lead to a bad user experience when a pod with running sessions will be removed
at the moment there is no easy way to down-scale a specific port
- maybe it will be possible in the future to use scores as a weight for the HPA decision which pod should be removed (if so the implementation of k8sTicket will be adjusted)
- you can't ensure how the users will be distributed across the several Pods in your cluster
	- there is no easy way to tell the user that there are no more resources left
	- sometimes you want to limit the maximal number of users to only one (e.g. for debugging or for data privacy reasons)

k8sTicket tries to overcome those limitations by implementing a ticket system for the users. Each user who wants to use your application will take a ticket and get their resources. The users themselves won't even notice the system (with exception when all resources are in use).

## Technical Details
k8sTicket has two main components: A reverse proxy based on Gorilla web toolkit and the Kubernetes custom controller based on the client-go library.
k8sTicket can only handle stateful web applications with continuous Server-Client-communication like WebSocket- or XHR-applications. It was tested successfully with R-Shiny apps.

![](k8sTicket.png)

### How does it work?
When being started k8sTicket will configure itself by reading Kubernetes metadata annotations and labels.
It will recognize Deployments it should deliver and the associated Pods. Afterwards, it will calculate the number of available tickets.
When a client connects to the service a WebSocket connection to the k8sTicket server will be established by javascript. The server handles the query and checks for available resources. If there are free resources a new ticket will be generated and given to the client (as a session cookie). Then the client will be redirected to an address proxied by k8sTicket delivering the service of the Pod.
Whenever a new ticket is generated k8sTicket checks if there are still enough spare tickets (can be configured) for new clients. If not k8sTicket will spawn new Pods in Kubernetes. Those Pods will be also removed if they are not needed anymore (based on a threshold value). A ticket will be marked as active as long as there is an established HTTP connection (or HTTP connections in short intervals).

### Configuration
k8sTicket can deliver services of one or more Deployments. Please note that for each service a different port must be used in the configuration at the moment. Please also note that k8sTicket is limited to the namespace it is running in.
Examples are provided in ....
The good news is that k8sTicket can be easily used with your existing Deployments without too much reconfiguration. Because k8sTicket will deliver your application content, you do not need ingress definitions for them anymore, just for k8sTicket.

**How to configure k8sTicket?**

k8sTicket retrieves its configuration directly from the metadata definitions of the target Deployment(s).

The following Labels and Annotations can be used in your definitions:

#### Labels:
##### Deployment:

`k8sTicket: "true"`

Enables k8sTicket for this Deployment
Setting it to any other value than "true" will stop k8sTicket on this service gracefully
This means existing connections will be served until the user quits; new connections are not accepted anymore
Warning: If there are still scaled Pods by k8sTicket, you have to remove them by yourself

##### Pods (PodTemplate of the Deployment):

`ipb-halle.de/k8sticket.deployment.app: name_of_your_service`

This label is needed by k8sTicket to recognize which Pods belong to which applications. It must be the same as the annotation `ipb-halle.de/k8sticket.deployment.app` at the deployment (following).


#### Annotations:

##### Deployments:

`ipb-halle.de/k8sticket.deployment.app: name_of_your_service`

Must be the same as the label with the same name on the Pod. This name must also be unique in the namespace.
k8sTicket will use this name as path to deliver your application in the proxy component.

`ipb-halle.de/k8sticket.deployment.maxPods: "1"`

The number of Pods that k8sTicket is allowed to scale in this namespace.
Default: "1"

`ipb-halle.de/k8sticket.deployment.maxTickets: "1"`

The maximal number of tickets (users) for each Pod.

`ipb-halle.de/k8sticket.deployment.port: "9001"`

The port of k8sTicket which will be used to serve your application. Must be unique for each k8sTicket instance in a namespace.
Default: "1"

`ipb-halle.de/k8sticket.deployment.Podcooldown: "10"`

The time in seconds until an unused Pod (without tickets) will be down-scaled by k8sTicket. Please note that this will also modify the interval of movement checks. That means in practice that if the interval is 10 seconds a Pod can be removed between 10 and 20 seconds after it was abandoned.
Default: "10"

`ipb-halle.de/k8sticket.deployment.spareTickets: "2"`

The tickets that should be available for new users. k8sTicket will spawn as many Pods as needed for this number of unoccupied tickets until `ipb-halle.de/k8sticket.deployment.maxPods` is reached.


##### Pods (PodTemplate of the Deployment):

`ipb-halle.de/k8sTicket.port: "80"`

The port where your application is served in the Pod.
Default: "80"

`ipb-halle.de/k8sTicket.path`

The HTTP path of your application in the Pod. k8sTicket will rewrite the requests to this path.
Default: "/"

## Metric

k8sTicket has an endpoint for prometheus metrics. At the moment it is running at port 9999/metrics, but it will be changed in the future.
The following metrics are exported:

##### Gauges

`k8sticket_current_users_total`

The total number of current users with a valid Ticket.

`k8sticket_current_free_tickets_total`

The number of slots than can be used for client connections (unused Tickets).

`k8sticket_scaled_pods_total`

The number of pods autoscaled by k8sticket.

##### Counters

`k8sticket_users_total`

The total number of users served (total number of made out Tickets).


## Questions:
**Can I add another Deployment/ single Pods to an existing Deployment that is already handled by k8sTicket?**

Yes, you just need to add necessary Labels and Annotations to the PodSpec. In that way, you can deliver for example different versions of your applications.


**How to modify the path (application name) of a running deployment?**

Without downtime:

You have the option to copy your existing deployment as it is and modify the necessary Labels and Annotations (please note that you must modify the port). k8sTicket will then serve this application in addition. Remove "true" in the metadata annotation of the label k8sTicket in your old Deployment definition. k8sTicket will still serve the existing connections, but will not allow new connections at the old endpoint. When all connections on the old endpoint are gone, you can remove the Deployment entirely.

With downtime:

When a downtime is acceptable, you can just remove "true" at the k8sTicket Label, wait until all your clients disconnect and rename the Annotation for the Deployment and the Label for the Pods. Please note, that renaming the Label for the Pods will cause Kubernetes to replace all running Pods (that is the reason for removing "true" at the k8sTicket-Label first).
