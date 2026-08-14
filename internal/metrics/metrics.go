package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	IngestRunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "galning_ingest_run_total",
			Help: "Total number of Ingest Runs, labelled by result (success or failure).",
		},
		[]string{"result"},
	)

	EventsArchivedTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "galning_ingest_events_archived_total",
			Help: "Total number of Audit Events written to the Archive across all Ingest Runs.",
		},
	)
)

// RegisterIngest registers the ingest metrics with the default Prometheus registry.
func RegisterIngest() {
	prometheus.MustRegister(IngestRunsTotal, EventsArchivedTotal)
}
