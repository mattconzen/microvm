package obs

import (
	"os"
	"sync"

	"github.com/DataDog/datadog-go/v5/statsd"
)

const (
	MetricCreate         = "microvm.create.duration_ms"
	MetricColdStart      = "microvm.cold_start.duration_ms"
	MetricExec           = "microvm.exec.duration_ms"
	MetricCp             = "microvm.cp.duration_ms"
	MetricCpBytes        = "microvm.cp.bytes"
	MetricShellSession   = "microvm.shell.session_duration_ms"
	MetricSnapshot       = "microvm.snapshot.duration_ms"
	MetricCheckpoint     = "microvm.checkpoint.duration_ms"
	MetricResume         = "microvm.resume.duration_ms"
	MetricTerminate      = "microvm.terminate.duration_ms"
	MetricLogin          = "microvm.login.duration_ms"
	MetricAPICall        = "microvm.api_call.duration_ms"
	MetricCountSuffix    = ".count"
)

type Metrics interface {
	Histogram(name string, value float64, tags []string, rate float64) error
	Incr(name string, tags []string, rate float64) error
	Close() error
}

type noopMetrics struct{}

func (noopMetrics) Histogram(string, float64, []string, float64) error { return nil }
func (noopMetrics) Incr(string, []string, float64) error               { return nil }
func (noopMetrics) Close() error                                       { return nil }

var (
	once   sync.Once
	client Metrics = noopMetrics{}
)

func InitMetrics(version string) Metrics {
	once.Do(func() {
		addr := os.Getenv("STATSD_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8125"
		}
		c, err := statsd.New(addr,
			statsd.WithNamespace(""),
			statsd.WithTags([]string{"cli_version:" + version}),
		)
		if err != nil {
			// statsd unreachable -> stay no-op; CLI still works offline.
			return
		}
		client = c
	})
	return client
}

func M() Metrics { return client }
