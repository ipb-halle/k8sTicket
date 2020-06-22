# Simple Example
This is a simple example showing a configuration of k8sTicket. 
The deployed example application is [GoldenMutagenesisWeb](https://msbi.ipb-halle.de/GoldenMutagenesis/). 

# Files
## 01_rbac.yaml

This yaml sets up the [RBAC](https://kubernetes.io/docs/reference/access-authn-authz/rbac/) for k8sTicket's kubernetes-controller component. 
As you can see k8sTicket has all privileges on Pods, it is allowed to get, watch and list Deployments and also to get ObjectMetaData.

## 02_k8sTicket.yaml
This will deploy k8sTicket in the example namespace, waiting for other Deployments.

## 03_gmweb.yaml
This will deploy GoldenMutagenesisWeb and configure it for the with k8sTicket.
