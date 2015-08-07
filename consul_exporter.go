package main

import (
	"flag"
	"net/http"
	_ "net/http/pprof"
	"regexp"
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/log"

	consul_api "github.com/hashicorp/consul/api"
	consul "github.com/hashicorp/consul/consul/structs"
)

const (
	namespace = "consul"
)

var (
	serviceLabelNames = []string{"service", "node"}
	memberLabelNames  = []string{"member"}
)

// Exporter collects Consul stats from the given server and exports them using
// the prometheus metrics package.
type Exporter struct {
	URI   string
	mutex sync.RWMutex

	up, clusterServers                                            prometheus.Gauge
	nodeCount, serviceCount                                       prometheus.Counter
	serviceNodesTotal, serviceNodesHealthy, nodeChecks, keyValues *prometheus.GaugeVec
	serviceEntriesNodesTotal, serviceEntriesNodesHealthy          *prometheus.GaugeVec
	client                                                        *consul_api.Client
	kvPrefix                                                      string
	kvFilter                                                      *regexp.Regexp
}

// NewExporter returns an initialized Exporter.
func NewExporter(uri string, kvPrefix string, kvFilter string) *Exporter {
	// Set up our Consul client connection.
	consul_client, _ := consul_api.NewClient(&consul_api.Config{
		Address: uri,
	})

	// Init our exporter.
	return &Exporter{
		URI: uri,
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Was the last query of Consul successful.",
		}),

		clusterServers: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "raft_peers",
			Help:      "How many peers (servers) are in the Raft cluster.",
		}),

		nodeCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "serf_lan_members",
			Help:      "How many members are in the cluster.",
		}),

		serviceCount: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "catalog_services",
			Help:      "How many services are in the cluster.",
		}),

		serviceNodesTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "catalog_service_nodes",
				Help:      "Number of nodes currently registered for this service.",
			},
			[]string{"service"},
		),

		serviceNodesHealthy: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "catalog_service_node_healthy",
				Help:      "Is this service healthy on this node?",
			},
			[]string{"service", "node"},
		),

		serviceEntriesNodesTotal: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "catalog_service_entries_by_nodes",
				Help:      "Number of service entries currently registered by node for this service.",
			},
			[]string{"service", "node"},
		),

		serviceEntriesNodesHealthy: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "catalog_service_entries_by_node_healthy",
				Help:      "Number of service entries currently registered by node that are healthy for this service.",
			},
			[]string{"service", "node"},
		),

		nodeChecks: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "agent_check",
				Help:      "Is this check passing on this node?",
			},
			[]string{"check", "node"},
		),

		keyValues: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Name:      "catalog_kv",
				Help:      "The values for selected keys in Consul's key/value catalog. Keys with non-numeric values are omitted.",
			},
			[]string{"key"},
		),

		client:   consul_client,
		kvPrefix: kvPrefix,
		kvFilter: regexp.MustCompile(kvFilter),
	}
}

// Describe describes all the metrics ever exported by the Consul exporter. It
// implements prometheus.Collector.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- e.up.Desc()
	ch <- e.nodeCount.Desc()
	ch <- e.serviceCount.Desc()
	ch <- e.clusterServers.Desc()

	e.serviceNodesTotal.Describe(ch)
	e.serviceNodesHealthy.Describe(ch)
	e.serviceEntriesNodesTotal.Describe(ch)
	e.serviceEntriesNodesHealthy.Describe(ch)
	e.keyValues.Describe(ch)
}

// Collect fetches the stats from configured Consul location and delivers them
// as Prometheus metrics. It implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	services := make(chan []*consul_api.ServiceEntry)
	checks := make(chan []*consul_api.HealthCheck)

	go e.queryClient(services, checks)

	e.mutex.Lock() // To protect metrics from concurrent collects.
	defer e.mutex.Unlock()

	// Reset metrics.
	e.serviceNodesTotal.Reset()
	e.serviceNodesHealthy.Reset()
	e.serviceEntriesNodesTotal.Reset()
	e.serviceEntriesNodesHealthy.Reset()
	e.nodeChecks.Reset()

	e.setMetrics(services, checks)

	ch <- e.up
	ch <- e.clusterServers
	ch <- e.nodeCount
	ch <- e.serviceCount

	e.serviceNodesTotal.Collect(ch)
	e.serviceNodesHealthy.Collect(ch)
	e.serviceEntriesNodesTotal.Collect(ch)
	e.serviceEntriesNodesHealthy.Collect(ch)
	e.nodeChecks.Collect(ch)

	e.keyValues.Reset()
	e.setKeyValues()
	e.keyValues.Collect(ch)
}

func (e *Exporter) queryClient(services chan<- []*consul_api.ServiceEntry, checks chan<- []*consul_api.HealthCheck) {

	defer close(services)
	defer close(checks)

	// How many peers are in the Consul cluster?
	peers, err := e.client.Status().Peers()

	if err != nil {
		e.up.Set(0)
		log.Errorf("Query error is %v", err)
		return
	}

	// We'll use peers to decide that we're up.
	e.up.Set(1)
	e.clusterServers.Set(float64(len(peers)))

	// How many nodes are registered?
	nodes, _, err := e.client.Catalog().Nodes(&consul_api.QueryOptions{})

	if err != nil {
		// FIXME: How should we handle a partial failure like this?
	} else {
		e.nodeCount.Set(float64(len(nodes)))
	}

	// Query for the full list of services.
	serviceNames, _, err := e.client.Catalog().Services(&consul_api.QueryOptions{})
	e.serviceCount.Set(float64(len(serviceNames)))

	if err != nil {
		// FIXME: How should we handle a partial failure like this?
		return
	}

	e.serviceCount.Set(float64(len(serviceNames)))

	for s := range serviceNames {
		s_entries, _, err := e.client.Health().Service(s, "", false, &consul_api.QueryOptions{})

		if err != nil {
			log.Errorf("Failed to query service health: %v", err)
			continue
		}

		services <- s_entries
	}

	c_entries, _, err := e.client.Health().State("any", &consul_api.QueryOptions{})
	if err != nil {
		log.Errorf("Failed to query service health: %v", err)

	} else {
		checks <- c_entries
	}

}

func (e *Exporter) setMetrics(services <-chan []*consul_api.ServiceEntry, checks <-chan []*consul_api.HealthCheck) {

	// Each service will be an array of ServiceEntry structs.
	running := true
	for running {
		select {
		case service, b := <-services:
			running = b
			if len(service) == 0 {
				// Not sure this should ever happen, but catch it just in case...
				continue
			}

			// We should have one ServiceEntry per node, so use that for total nodes.
			// NOTE: The above statement is false.  You can run multiple ServiceEntries per node.
			log.Printf("Service: %v", service[0].Service)
			log.Printf("Service len: %d", len(service))
			e.serviceNodesTotal.WithLabelValues(service[0].Service.Service).Set(float64(len(service)))

			//e.serviceEntriesNodesTotal.Collect(ch)
			//e.serviceEntriesNodesHealthy.Collect(ch)
			var serviceEntriesNodesTotal map[string]int
			var serviceEntriesNodesHealthy map[string]int
			serviceEntriesNodesTotal = make(map[string]int)
			serviceEntriesNodesHealthy = make(map[string]int)

			for _, entry := range service {
				// We have a Node, a Service, and one or more Checks. Our
				// service-node combo is passing if all checks have a `status`
				// of "passing."

				passing := 1

				for _, hc := range entry.Checks {
					if hc.Status != consul.HealthPassing {
						passing = 0
						break
					}
				}

				serviceNode := entry.Service.Service + "-" + entry.Node.Node

				serviceNodeTotal := serviceEntriesNodesTotal[serviceNode] + 1
				serviceEntriesNodesTotal[serviceNode] = serviceNodeTotal

				serviceNodeHealthy := serviceEntriesNodesHealthy[serviceNode]
				if passing == 1 {
					serviceNodeHealthy++
					serviceEntriesNodesHealthy[serviceNode] = serviceNodeHealthy
				}

				log.Infof("%v/%v status is %v", entry.Service.Service, entry.Node.Node, passing)

				e.serviceNodesHealthy.WithLabelValues(entry.Service.Service, entry.Node.Node).Set(float64(passing))
				e.serviceEntriesNodesTotal.WithLabelValues(entry.Service.Service, entry.Node.Node).Set(float64(serviceNodeTotal))
				e.serviceEntriesNodesHealthy.WithLabelValues(entry.Service.Service, entry.Node.Node).Set(float64(serviceNodeHealthy))
			}

		case entry, b := <-checks:
			running = b
			for _, hc := range entry {
				passing := 1
				if hc.ServiceID == "" {
					if hc.Status != consul.HealthPassing {
						passing = 0
					}
					e.nodeChecks.WithLabelValues(hc.CheckID, hc.Node).Set(float64(passing))
					log.Infof("CHECKS: %v/%v status is %d", hc.CheckID, hc.Node, passing)
				}
			}
		}
	}

}

func (e *Exporter) setKeyValues() {
	if e.kvPrefix == "" {
		return
	}

	kv := e.client.KV()

	pairs, _, err := kv.List(e.kvPrefix, &consul_api.QueryOptions{})
	if err != nil {
		log.Errorf("Error fetching key/values: %s", err)
		return
	}

	for _, pair := range pairs {
		if e.kvFilter.MatchString(pair.Key) {
			val, err := strconv.ParseFloat(string(pair.Value), 64)
			if err == nil {
				e.keyValues.WithLabelValues(pair.Key).Set(val)
			}
		}
	}
}

func main() {
	var (
		listenAddress = flag.String("web.listen-address", ":9107", "Address to listen on for web interface and telemetry.")
		metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
		consulServer  = flag.String("consul.server", "localhost:8500", "HTTP API address of a Consul server or agent.")
		kvPrefix      = flag.String("kv.prefix", "", "Prefix from which to expose key/value pairs.")
		kvFilter      = flag.String("kv.filter", ".*", "Regex that determines which keys to expose.")
	)
	flag.Parse()

	exporter := NewExporter(*consulServer, *kvPrefix, *kvFilter)
	prometheus.MustRegister(exporter)

	log.Infof("Starting Server: %s", *listenAddress)
	http.Handle(*metricsPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
             <head><title>Consul Exporter</title></head>
             <body>
             <h1>Consul Exporter</h1>
             <p><a href='` + *metricsPath + `'>Metrics</a></p>
             </body>
             </html>`))
	})
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
