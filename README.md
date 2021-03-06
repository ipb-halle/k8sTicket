# k8sTicket
![GitHub](https://img.shields.io/github/license/ipb-halle/k8sticket)
![CI](https://github.com/ipb-halle/k8sTicket/workflows/CI/badge.svg)
[![Go Report Card](https://goreportcard.com/badge/github.com/ipb-halle/k8sTicket)](https://goreportcard.com/report/github.com/ipb-halle/k8sTicket)
![Docker Cloud Build Status](https://img.shields.io/docker/cloud/build/ipbhalle/k8sticket)



A in-Kubernetes load-balancing controller. Its' objective is to scale stateful HTTP applications based on the number of users in Kubernetes. It is mainly designed for applications using WebSockets, but should also support XHR applications.  

**The current implementation is still a beta version. It is being tested in the moment.**

## Documentation
The documentation and complete explanations are available [in this document](docs/Documentation.md).

## Examples
Configuration examples are available [here (simple example)](examples/simple_example) and [here (advanced example)](examples/advanced_example). A tutorial how to use k8sTicket with an existing Deployment can be found [here](docs/HowTo.md).

## Funding
Developed by Chris Ulpinnis for Leibniz-Institute of Plant Biochemistry (IPB), funded by grant de.NBI 031L0107 Metabolite Annotation & Sharing Halle (MASH).

<img src="https://raw.githubusercontent.com/ipb-halle/k8sTicket/master/docs/denbi-logo-color.svg?sanitize=true" width="30%">
