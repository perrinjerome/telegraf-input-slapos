package slapos

import (
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/testutil"
	"github.com/stretchr/testify/require"
)

type DummySupervisor struct {
	instanceRoot string
	xMLRPCServer *httptest.Server
	socket       net.Listener
	processes    []*os.Process
}

// make dummy XML-RPC server on unix socket, living in instance root
func NewDummySupervisor(instanceRoot string) (*DummySupervisor, error) {
	var processes []*os.Process

	socket_path := filepath.Join(instanceRoot, "sv.sock")

	sock, err := net.Listen("unix", socket_path)
	if err != nil {
		return nil, err
	}
	procAttr := new(os.ProcAttr)
	procAttr.Files = []*os.File{os.Stdin, os.Stdout, os.Stderr}
	prog1, err := os.StartProcess("/bin/sleep", []string{"/bin/sleep", "10"}, procAttr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Could not start dummy process %v\n", err)
	}
	processes = append(processes, prog1)

	server := httptest.NewUnstartedServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			data, err := ioutil.ReadFile("testdata/xmlrpc_response.xml")
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: Could not read xmlrpc_response.xml %v\n", err)
			}
			responseData := string(data)
			responseData = strings.ReplaceAll(responseData, "@@PID_PROG1@@", fmt.Sprint(prog1.Pid))
			responseData = strings.ReplaceAll(responseData, "@@PID_PROG2@@", fmt.Sprint(prog1.Pid))
			fmt.Fprint(w, responseData)
		}))
	server.Listener = sock
	server.Start()

	return &DummySupervisor{instanceRoot, server, sock, processes}, nil
}

func (s *DummySupervisor) Close() {
	s.socket.Close()
	s.xMLRPCServer.Close()
	for _, process := range s.processes {
		process.Kill() //nolint:errcheck
	}
}

// return the metrics collected by acc.GetTelegrafMetrics, but after replacing runtime dependent
// metrics with 0. This also checks that the metrics are present
func getNormalizedTelegrafMetrics(t *testing.T, acc *testutil.Accumulator) []telegraf.Metric {
	metrics := acc.GetTelegrafMetrics()
	sort.Slice(metrics, func(i, j int) bool {
		return metrics[i].Tags()["reference"]+metrics[i].Tags()["name"] < metrics[j].Tags()["reference"]+metrics[j].Tags()["name"]
	})
	for _, metric := range metrics {
		if v, ok := metric.GetField("state"); ok && v.(SupervisorProcessState) == RUNNING {
			// metrics from running process
			for _, metricName := range []string{
				"cpu_times_user",
				"cpu_times_system",
				"cpu_times_idle",
				"cpu_times_nice",
				"cpu_times_iowait",
				"cpu_times_irq",
				"cpu_times_softirq",
				"cpu_times_steal",
				"cpu_times_guest",
				"cpu_times_guest_nice",
				"mem_rss",
				"mem_vms",
				"mem_hwm",
				"mem_data",
				"mem_stack",
				"mem_locked",
				"mem_swap",
			} {
				require.True(t, metric.HasField(metricName), "missing field %v in %q", metricName, metric)
				metric.RemoveField(metricName)
			}
			if !strings.Contains(runtime.GOOS, "darwin") {
				for _, metricName := range []string{
					"num_fds",
					"io_counters_read_count",
					"io_counters_read_bytes",
					"io_counters_write_count",
					"io_counters_write_bytes",
				} {
					require.True(t, metric.HasField(metricName), "missing field %v in %q", metricName, metric)
					metric.RemoveField(metricName)
				}
			}
		}
	}
	return metrics
}

func TestSlapOSGeneratesMetrics(t *testing.T) {
	tmpdir := t.TempDir()
	server, err := NewDummySupervisor(tmpdir)
	if err != nil {
		t.Fatalf("Cannot start server %v", err)
	}
	defer server.Close()

	r := SlapOSMetricCollector{
		InstanceRoot: tmpdir,
		SocketName:   "sv.sock",
		Log:          testutil.Logger{},
	}
	err = r.Init()
	require.NoError(t, err)
	var acc testutil.Accumulator

	err = r.Gather(&acc)
	require.NoError(t, err)
	require.Empty(t, acc.Errors)

	metrics := getNormalizedTelegrafMetrics(t, &acc)

	testutil.RequireMetricsEqual(t,
		[]telegraf.Metric{
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1",
					"name":      "prog1",
				},
				map[string]interface{}{
					"state":       20,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1",
					"name":      "prog2",
				},
				map[string]interface{}{
					"state":       20,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1",
					"name":      "prog3",
				},
				map[string]interface{}{
					"state":       0,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			)}, metrics, testutil.IgnoreTime())
}
func TestSlapOSGeneratesMetricsFilterProcessNames(t *testing.T) {
	tmpdir := t.TempDir()
	server, err := NewDummySupervisor(tmpdir)
	if err != nil {
		t.Fatalf("Cannot start server %v", err)
	}
	defer server.Close()

	r := SlapOSMetricCollector{
		InstanceRoot:        tmpdir,
		IgnoredProcessNames: []string{"prog1", "prog2"},
		SocketName:          "sv.sock",

		Log: testutil.Logger{},
	}
	err = r.Init()
	require.NoError(t, err)
	var acc testutil.Accumulator

	err = r.Gather(&acc)
	require.NoError(t, err)
	require.Empty(t, acc.Errors)

	metrics := getNormalizedTelegrafMetrics(t, &acc)

	testutil.RequireMetricsEqual(t,
		[]telegraf.Metric{
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1",
					"name":      "prog3",
				},
				map[string]interface{}{
					"state":       0,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			)}, metrics, testutil.IgnoreTime())
}

func TestSlapOSGeneratesMetricsRecursively(t *testing.T) {
	tmpdir := t.TempDir()
	server, err := NewDummySupervisor(tmpdir)
	if err != nil {
		t.Fatalf("Cannot start server %v", err)
	}
	defer server.Close()
	partition := filepath.Join(tmpdir, "slappart1", "srv", "runner", "instance")
	err = os.MkdirAll(partition, 0700)
	if err != nil {
		t.Fatalf("Cannot create partition %v", err)
	}
	partitionServer, err := NewDummySupervisor(partition)
	if err != nil {
		t.Fatalf("Cannot start sub-partition server %v", err)
	}
	defer partitionServer.Close()

	r := SlapOSMetricCollector{
		InstanceRoot:                 tmpdir,
		SocketName:                   "sv.sock",
		RecursiveInstanceGlobPattern: "*/srv/runner/inst*",
		Log:                          testutil.Logger{},
	}
	err = r.Init()
	require.NoError(t, err)
	var acc testutil.Accumulator

	err = r.Gather(&acc)
	require.NoError(t, err)
	require.Empty(t, acc.Errors)

	metrics := getNormalizedTelegrafMetrics(t, &acc)

	testutil.RequireMetricsEqual(t,
		[]telegraf.Metric{

			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1:slappart1",
					"name":      "prog1",
				},
				map[string]interface{}{
					"state":       20,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1:slappart1",
					"name":      "prog2",
				},
				map[string]interface{}{
					"state":       20,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1:slappart1",
					"name":      "prog3",
				},
				map[string]interface{}{
					"state":       0,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1",
					"name":      "prog1",
				},
				map[string]interface{}{
					"state":       20,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1",
					"name":      "prog2",
				},
				map[string]interface{}{
					"state":       20,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
			testutil.MustMetric(
				"slapos",
				map[string]string{
					"reference": "slappart1",
					"name":      "prog3",
				},
				map[string]interface{}{
					"state":       0,
					"exit_status": 0,
					"start":       1624113313,
				},
				time.Unix(0, 0),
			),
		}, metrics, testutil.IgnoreTime())

}
