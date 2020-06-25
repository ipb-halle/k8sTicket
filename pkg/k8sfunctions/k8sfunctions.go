package k8sfunctions

import (
	"errors"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/culpinnis/k8sTicket/pkg/proxyfunctions"
	"k8s.io/api/core/v1"
)

const (
	defaultPort = 80
	defaultPath = "/"
)

// Namespace This function returns the namespace of the running pod
func Namespace() string {
	// This way assumes you've set the POD_NAMESPACE environment variable using the downward API.
	// This check has to be done first for backwards compatibility with the way InClusterConfig was originally set up
	if ns, ok := os.LookupEnv("POD_NAMESPACE"); ok {
		return ns
	}

	// Fall back to the namespace associated with the service account token, if available
	if data, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(data)); len(ns) > 0 {
			return ns
		}
	}

	return "default"
}

// PodToConfig This function reads the k8sTicket annotations and creates
// a config for the reverse proxy.
func PodToConfig(pod *v1.Pod) (proxyfunctions.Config, error) {
	ip := pod.Status.PodIP
	if ip == "" {
		return proxyfunctions.Config{}, errors.New("pod has no vaild IP")
	}
	cpath := defaultPath
	cport := defaultPort
	if path, ok := pod.GetAnnotations()["ipb-halle.de/k8sTicket.path"]; ok {
		cpath = path
	}
	if port, ok := pod.GetAnnotations()["ipb-halle.de/k8sTicket.port"]; ok {
		_, err := strconv.Atoi(port)
		if err != nil {
			log.Println("k8s: Annotation: ", pod.GetName(), ": ", port, ": ", err, " Default ", defaultPort, "will be used")
			cport = defaultPort
		} else {
			cport, _ = strconv.Atoi(port)
		}
	}
	return proxyfunctions.Config{Host: ip + ":" + strconv.Itoa(cport), Path: cpath}, nil
}
