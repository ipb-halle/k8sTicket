package k8sfunctions

import (
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//PMetric This struct defines our exported metrics.
// We export the current users, the available Tickets, the scaled Pods and
// a counter for all served users.
type PMetric struct {
	CurrentUsers       *prometheus.GaugeVec
	CurrentFreeTickets *prometheus.GaugeVec
	CurrentScaledPods  *prometheus.GaugeVec
	TotalUsers         *prometheus.CounterVec
}

//NewPMetric This function defines the metrics from the PMetric struct.
// We will label each value with the Prefix (application name).
func NewPMetric() PMetric {
	return (PMetric{
		CurrentUsers: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "k8sticket_current_users_total",
			Help: "The total number of current users",
		},
			[]string{"application"}),
		CurrentFreeTickets: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "k8sticket_current_free_tickets_total",
			Help: "The number of slots than can be used for client connections",
		},
			[]string{"application"}),
		CurrentScaledPods: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "k8sticket_scaled_pods_total",
			Help: "The number of pods autoscaled by k8sticket",
		},
			[]string{"application"}),
		TotalUsers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "k8sticket_users_total",
			Help: "The total number of users served (total number of made out tickets)",
		},
			[]string{"application"}),
	})
}

//UpdateAccessMetric This method uses the Serverlists ability to export informer channels
// to inform external functions. The metrics are updated whenever a new server or
// a new Ticket are registered or when a Ticket/server is removed.
func (proxy *ProxyForDeployment) UpdateAccessMetric(informer chan string) {
	for {
		select {
		case msg := <-informer:
			if msg == "new ticket" {
				proxy.metric.TotalUsers.WithLabelValues(proxy.Serverlist.Prefix).Inc()
			}
			proxy.metric.CurrentUsers.WithLabelValues(proxy.Serverlist.Prefix).Set(float64(proxy.Serverlist.GetTickets()))
			proxy.metric.CurrentFreeTickets.WithLabelValues(proxy.Serverlist.Prefix).Set(float64(proxy.Serverlist.GetAvailableTickets()))
		case <-proxy.metricStopper:
			return
		}
	}
}

//UpdatePodMetric This method is called in the PodHandler to update the metric
// about new or deleted autoscaled Pods by k8sTicket
func (proxy *ProxyForDeployment) UpdatePodMetric() {
	pods, err := proxy.Clientset.CoreV1().Pods(proxy.Namespace).List(metav1.ListOptions{LabelSelector: "ipb-halle.de/k8sticket.deployment.app=" + proxy.Serverlist.Prefix + ",ipb-halle.de/k8sTicket.scaled=true"})
	if err != nil {
		panic("Metric: UpdatePodMetric: " + err.Error())
	}
	proxy.metric.CurrentScaledPods.WithLabelValues(proxy.Serverlist.Prefix).Set(float64(len(pods.Items)))
}
