# Simple Example
This is a simple example showing a configuration of k8sTicket.
The deployed example application is [GoldenMutagenesisWeb](https://msbi.ipb-halle.de/GoldenMutagenesis/) in the namespace "example".

# Files
## 1_rbac.yaml

This yaml sets up the [RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/) for k8sTicket's kubernetes-controller component.
As you can see k8sTicket has all privileges on Pods, it is allowed to get, watch and list Deployments and also to get ObjectMetaData.

## 2_k8sTicket.yaml
This will deploy k8sTicket in the example namespace, waiting for other Deployments.

## 3_k8sTicket_service.yaml
Creates the service definition for k8sTicket, including the port 9001 for the GoldenMutagenesisWeb application.

## 4_gmweb.yaml
This will deploy GoldenMutagenesisWeb and configure it for the use with k8sTicket.

## 5_k8sTicket_ingress.yaml
This defines the ingress specification for our GoldenMutagenesisWeb application delivered by k8sTicket.
Please adjust the host setting according to your DNS.
