package k8sfunctions

import (
	"errors"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/culpinnis/k8sTicket/internal/pkg/proxyfunctions"
	"k8s.io/api/core/v1"
	//"k8s.io/apimachinery/pkg/api/errors"
	//
	// Uncomment to load all auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth"
	//
	// Or uncomment to load specific auth plugins
	// _ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/oidc"
	// _ "k8s.io/client-go/plugin/pkg/client/auth/openstack"
)

const (
	default_port = 80
	default_path = "/"
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
		return proxyfunctions.Config{}, errors.New("Pod has no vaild IP")
	}
	cpath := default_path
	cport := default_port
	if path, ok := pod.GetAnnotations()["ipb-halle.de/k8sTicket.path"]; ok {
		cpath = path
	}
	if port, ok := pod.GetAnnotations()["ipb-halle.de/k8sTicket.port"]; ok {
		_, err := strconv.Atoi(port)
		if err != nil {
			log.Println("k8s: Annotation: ", pod.GetName(), ": ", port, ": ", err, " Default ", default_port, "will be used")
			cport = default_port
		} else {
			cport, _ = strconv.Atoi(port)
		}
	}
	return proxyfunctions.Config{Host: ip + ":" + strconv.Itoa(cport), Path: cpath}, nil
}
