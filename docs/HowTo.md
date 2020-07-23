# Setting up k8sTicket on exisiting Deployments
In this tutorial we describe how to setup k8sTicket with applications that are already deployed in Kubernetes. 
## Existing configuration
We assume that your exisiting application is deployed at least with 3 configurations: A Deployment, a Service and an Ingress definition.
### DeploymentSpec
This yaml deploys the application GoldenMutagenesisWeb with two replicas in the namespace example in Kubernetes. 

````yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: goldenmutagenesisweb-deployment
  namespace: example
  spec:
  progressDeadlineSeconds: 600
  replicas: 2
  selector:
    matchLabels:
      app: gmweb
  strategy:
    rollingUpdate:
      maxSurge: 25%
      maxUnavailable: 25%
    type: RollingUpdate
  template:
    metadata:
      labels:
        app: gmweb
    spec:
      containers:
      - image: sneumann/goldenmutagenesisweb
        imagePullPolicy: Always
        livenessProbe:
          failureThreshold: 3
          httpGet:
            httpHeaders:
            - name: Host
              value: /
            path: /
            port: 3838
            scheme: HTTP
          initialDelaySeconds: 15
          periodSeconds: 35
          successThreshold: 1
          timeoutSeconds: 4
        name: goldenmutagenesisweb-deployment
        securityContext:
          allowPrivilegeEscalation: false
          capabilities: {}
          privileged: false
          readOnlyRootFilesystem: false
          runAsNonRoot: false
      restartPolicy: Always
      terminationGracePeriodSeconds: 30
````
### ServiceSpec
This yaml defines a service for the web application which listens on port 3838.

````yaml
apiVersion: v1
kind: Service
metadata:
  name: goldenmutagenesisweb-service
  namespace: example
  labels:
    app: "gmweb"
spec:
  ports:
  - name: http
    port: 80
    protocol: TCP
    targetPort: 3838
  selector:
    app: gmweb
  type: ClusterIP
````
### IngressSpec
This is the definition of the ingress to make the service available outside of the cluster. A rewrite rule is defined, because the application runs at / in the container while it should be availalbe at /GoldenMutagenesisWeb in public.

````yaml
apiVersion: extensions/v1beta1
kind: Ingress
metadata:
  annotations:
    nginx.ingress.kubernetes.io/affinity: cookie
    nginx.ingress.kubernetes.io/proxy-body-size: 500m
    nginx.ingress.kubernetes.io/proxy-read-timeout: "600"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
    nginx.ingress.kubernetes.io/session-cookie-path: /GoldenMutagenesisWeb/
  name: goldenmutagenesisweb-ingress
  namespace: example
spec:
  rules:
  - host: example.com
    http:
      paths:
      - backend:
          serviceName: goldenmutagenesisweb-service
          servicePort: 80
        path: /GoldenMutagenesisWeb(/|$)(.*)
````
## Modifications to existing configuration
Now we will modify the configuration to use k8sTicket with the exisiting Deployment.
### DeploymentSpec
Specific Labels and Annotations must be added to the Deployment. A general list of useable Labels and Annotations can be found [here](Documentation.md#labels).

````yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: goldenmutagenesisweb-deployment
  namespace: example
  labels:
     ipb-halle.de/k8sticket: "true"
     app: gmweb
  annotations:
     ipb-halle.de/k8sticket.deployment.app.name: GoldenMutagenesisWeb
spec:
  progressDeadlineSeconds: 600
  replicas: 2
  selector:
    matchLabels:
      app: gmweb
  strategy:
    rollingUpdate:
      maxSurge: 25%
      maxUnavailable: 25%
    type: RollingUpdate
  template:
    metadata:
      labels:
        ipb-halle.de/k8sticket.deployment.app.name: GoldenMutagenesisWeb
      annotations:
        ipb-halle.de/k8sTicket.port: "3838"
    spec:
      containers:
      - image: sneumann/goldenmutagenesisweb
        imagePullPolicy: Always
        name: goldenmutagenesisweb-deployment
        securityContext:
          allowPrivilegeEscalation: false
          capabilities: {}
          privileged: false
          readOnlyRootFilesystem: false
          runAsNonRoot: false
      restartPolicy: Always
      terminationGracePeriodSeconds: 30
````

We have added `ipb-halle.de/k8sticket.deployment.app.name: GoldenMutagenesisWeb` as Label at the Pod template definition and as Annotation at the Deployment metadata. Defining this value twice is required. Furthermore, we added the Label `ipb-halle.de/k8sTicket: "true"` to the Deployment metadata. To tell k8sTicket the port of our example application we set `ipb-halle.de/k8sticket.port: "3838"` as Annotation to the Pod metadata template.
All other configuration values of k8sTicket will be kept default. That means k8sTicket will run at port 9001.
We kept the Label `app: gmweb` because the field selector is immutable in API version apps/v1.

### ServiceSpec
Instead of defining a Service for our GoldenMutagenesisWeb example application, a Service definition for k8sTicket is required. 

````yaml
apiVersion: v1
kind: Service
metadata:
  name: k8sticket-service
  namespace: example
spec:
  ports:
  - name: gmweb
    port: 9001
    protocol: TCP
    targetPort: 9001
  selector:
    app: k8sTicket
  sessionAffinity: None
  type: ClusterIP
````
We created a Service definition for port 9001 with the name gmweb. The service has a selector for the Label `app: k8sTicket`.
### IngressSpec
The Ingress definition will use the k8sticket-service instead of the (deleted) goldenmutagenesisweb-service.

````yaml
apiVersion: extensions/v1beta1
kind: Ingress
metadata:
  name: k8sticket-ingress
  namespace: example
spec:
  rules:
  - host: example.com
    http:
      paths:
      - backend:
          serviceName: k8sticket-service
          servicePort: gmweb
        path: /GoldenMutagenesisWeb
````
We have removed the rewrite instructions here, because k8sTicket will proxy and rewrite the requests. 

## k8sTicket Deployment
Finally we have to deploy k8sTicket in the example namspace. This step could also be the first one.
Because k8sTicket needs to read and write information to the cluster, it requires a ServiceAccount, a Role and a RoleBinding. For security reasons those definitions are limited to the example namespace.

````yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: k8sticket-watcher
  namespace: example

---

apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: k8sticket-role
  namespace: example
rules:
- apiGroups:
  - ""
  resources:
  - pods
  verbs:
  - get
  - watch
  - list
  - create
  - update
  - delete
- apiGroups:
  - ""
  resources:
  - deployments
  verbs:
  - get
  - watch
  - list
- apiGroups:
  - "apps"
  resources:
  - PartialObjectMetadata
  verbs:
  - list
  - get
  - watch

---

apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: k8sticket-rolebinding
  namespace: example
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: k8sticket-role
subjects:
- kind: ServiceAccount
  name: k8sticket-watcher

````
After creating the cluster permissions, k8sTicket can be deployed. 

````yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: k8sticket
  namespace: example
spec:
  progressDeadlineSeconds: 600
  replicas: 1
  revisionHistoryLimit: 10
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
    type: RollingUpdate
  template:
    metadata:
      labels:
        app: k8sTicket
    spec:
      containers:
      - image: ipb-halle/k8sticket
        imagePullPolicy: Always
        name: k8sticket
        ports:
        - containerPort: 9001
          name: serviceport
          protocol: TCP
        resources: {}
        securityContext:
          allowPrivilegeEscalation: false
          privileged: false
          readOnlyRootFilesystem: false
          runAsNonRoot: false
        stdin: true
        terminationMessagePath: /dev/termination-log
        terminationMessagePolicy: File
        tty: true
      dnsPolicy: ClusterFirst
      restartPolicy: Always
      schedulerName: default-scheduler
      securityContext: {}
      serviceAccount: k8sticket-watcher
      serviceAccountName: k8sticket-watcher
      terminationGracePeriodSeconds: 30
````
You can access our example application now. 