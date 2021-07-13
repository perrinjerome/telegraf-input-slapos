package slapos

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/kolo/xmlrpc"
	"github.com/shirou/gopsutil/v3/process"
)

// possible states for a process in supervisor XMLRPC-API
type SupervisorProcessState = int64

const (
	STOPPED  SupervisorProcessState = 0
	STARTING SupervisorProcessState = 10
	RUNNING  SupervisorProcessState = 20
	BACKOFF  SupervisorProcessState = 30
	STOPPING SupervisorProcessState = 40
	EXITED   SupervisorProcessState = 100
	FATAL    SupervisorProcessState = 200
	UNKNOWN  SupervisorProcessState = 1000
)

// Process Info returned by getAllProcessInfo XMLRPC call
type SupervisorProcessInfo struct {
	Name          string                 `xmlrpc:"name"`
	Group         string                 `xmlrpc:"group"`
	Start         int                    `xmlrpc:"start"`
	Stop          int                    `xmlrpc:"stop"`
	Now           int                    `xmlrpc:"now"`
	State         SupervisorProcessState `xmlrpc:"state"`
	Statename     string                 `xmlrpc:"statename"`
	SpawnErr      string                 `xmlrpc:"spawnerr"`
	ExitStatus    int                    `xmlrpc:"exitstatus"`
	LogFile       string                 `xmlrpc:"logfile"`
	StdoutLogFile string                 `xmlrpc:"stdout_logfile"`
	StderrLogFile string                 `xmlrpc:"stderr_logfile"`
	Pid           int32                  `xmlrpc:"pid"`
	Description   string                 `xmlrpc:"description"`
}

// Implement http.RoundTripper protocol for an unix socket
func NewUnixRoundTripper(socketPath string) *UnixSocketRoundTripper {
	return &UnixSocketRoundTripper{socketPath: socketPath, transport: &http.Transport{}}
}

type UnixSocketRoundTripper struct {
	socketPath string
	transport  *http.Transport
}

func (r UnixSocketRoundTripper) dialContext(ctx context.Context, network string, addr string) (net.Conn, error) {
	d := net.Dialer{}
	return d.DialContext(ctx, "unix", r.socketPath)
}

func (r UnixSocketRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.transport.DialContext = r.dialContext
	defer req.Body.Close()
	req.Close = true // don't keep connection open (otherwise we leak file descriptors)
	return r.transport.RoundTrip(req)
}

// Collect process metrics from running slapos node.
type SlapOSMetricCollector struct {
	InstanceRoot                 string   `toml:"instance_root"`
	SocketName                   string   `toml:"socket_name"`
	RecursiveInstanceGlobPattern string   `toml:"recursive_instance_glob_pattern"`
	IgnoredProcessNames          []string `toml:"ignored_process_names"`
	Debug                        bool     `toml:"debug"`

	ignoredProcessNameRegexps []regexp.Regexp
	cancel                    context.CancelFunc
	Log                       telegraf.Logger `toml:"-"`
}

func (s *SlapOSMetricCollector) Init() error {
	if len(s.InstanceRoot) == 0 {
		s.InstanceRoot = "/srv/slapgrid"
	}
	if len(s.SocketName) == 0 {
		s.SocketName = "sv.sock"
	}
	if len(s.RecursiveInstanceGlobPattern) == 0 {
		s.RecursiveInstanceGlobPattern = "*/srv/runner/inst*/"
	}
	for _, name := range s.IgnoredProcessNames {
		re, err := regexp.Compile(name)
		if err != nil {
			s.Log.Errorf("Unable to compile regular expression %s in ignored_process_names: %v", name, err)
		} else {
			s.ignoredProcessNameRegexps = append(s.ignoredProcessNameRegexps, *re)
		}
	}
	s.Log.Debugf(
		"Starting SlapOSMetricCollector from instance root: %v with pid %d\n",
		s.InstanceRoot,
		os.Getpid())
	return nil
}

func (s *SlapOSMetricCollector) SampleConfig() string {
	return `
    ## Folder where partitions are located
    # instance_root = "/srv/slapgrid/"

    ## filepath.Glob pattern to look for recursive instances
    # recursive_instance_glob_pattern = "*/srv/runner/inst*/"

    ## Path of supervisor socket, relative to instance root
    # socket_name = "sv.sock"
    
    ## Ignore processes whose name match these regular expressions
    # ignored_process_names = [
    #     "^certificate_authority.*",
    #     "^crond-.*",
    #     "^monitor-.*",
    #     "^bootstrap-monitor.*",
    #     "^watchdog$",
    # ]

	## Enable debug output
	debug = true
    `
}

func (s *SlapOSMetricCollector) Description() string {
	return "Provides basic metrics for process running on a slapos node"
}

// Gather metrics from a partition, and recurisvely from nested partitions
func (s *SlapOSMetricCollector) gatherFromPartition(
	a telegraf.Accumulator,
	partitionRoot string,
	partitionTagPrefix string,
) error {
	client, err := xmlrpc.NewClient(
		"http://hostname-ignored/RPC2",
		NewUnixRoundTripper(filepath.Join(partitionRoot, s.SocketName)))
	if err != nil {
		return err
	}
	defer client.Close()
	result := []SupervisorProcessInfo{}

	err = client.Call("supervisor.getAllProcessInfo", nil, &result)
	if err != nil {
		return err
	}

	for _, process := range result {
		if !s.isProcessIgnored((process)) {
			s.sendProcessMetric(a, process, partitionTagPrefix)
		}
	}

	matches, err := filepath.Glob(
		filepath.Clean(filepath.Join(partitionRoot, s.RecursiveInstanceGlobPattern, s.SocketName)))
	if err != nil {
		s.Log.Errorf("Error globbing recursive partitions: %v", err)
	} else {
		if len(partitionTagPrefix) > 0 {
			partitionTagPrefix = partitionTagPrefix + ":"
		}
		for _, socket := range matches {
			partitionName := strings.Split(strings.TrimPrefix(socket, partitionRoot+"/"), "/")[0]
			subPartitionRoot := filepath.Dir(socket)
			if subPartitionRoot != partitionRoot {
				err = s.gatherFromPartition(a, filepath.Dir(socket), partitionTagPrefix+partitionName+":")
				if err != nil {
					s.Log.Errorf("Error gather from recursive partitions: %v", err)
				}
			}
		}
	}

	return nil
}

func (s *SlapOSMetricCollector) debugf(format string, v ...interface{}) {
	if s.Debug {
		s.Log.Debugf(format, v...)
	}
}

func (s *SlapOSMetricCollector) isProcessIgnored(p SupervisorProcessInfo) bool {
	for _, re := range s.ignoredProcessNameRegexps {
		if re.Match([]byte(p.Name)) {
			return true
		}
	}
	return false
}

func (s *SlapOSMetricCollector) Gather(a telegraf.Accumulator) error {
	return s.gatherFromPartition(a, filepath.Clean(s.InstanceRoot), "")
}

func (s *SlapOSMetricCollector) Stop() {
	s.cancel()
}

func (s *SlapOSMetricCollector) sendProcessMetric(
	a telegraf.Accumulator,
	p SupervisorProcessInfo,
	partitionTagPrefix string,
) {
	fields := make(map[string]interface{})
	fields["state"] = SupervisorProcessState(p.State)
	fields["exit_status"] = p.ExitStatus
	fields["start"] = p.Start

	if SupervisorProcessState(p.State) == RUNNING {
		proc, err := process.NewProcess(p.Pid)
		if err != nil {
			s.debugf("Error getting process %v: %v", err, p.Pid)
		} else {
			cpu_times, err := proc.Times()
			if err != nil {
				s.debugf("Error getting CPU times %v: %v", err, p.Pid)
			} else {
				fields["cpu_times_user"] = cpu_times.User
				fields["cpu_times_system"] = cpu_times.System
				fields["cpu_times_idle"] = cpu_times.Idle
				fields["cpu_times_nice"] = cpu_times.Nice
				fields["cpu_times_iowait"] = cpu_times.Iowait
				fields["cpu_times_irq"] = cpu_times.Irq
				fields["cpu_times_softirq"] = cpu_times.Softirq
				fields["cpu_times_steal"] = cpu_times.Steal
				fields["cpu_times_guest"] = cpu_times.Guest
				fields["cpu_times_guest_nice"] = cpu_times.GuestNice
			}
			meminfo, err := proc.MemoryInfo()
			if err != nil {
				s.debugf("Error getting meminfo %v: %v", err, p.Pid)
			} else {
				fields["mem_rss"] = meminfo.RSS
				fields["mem_vms"] = meminfo.VMS
				fields["mem_hwm"] = meminfo.HWM
				fields["mem_data"] = meminfo.Data
				fields["mem_stack"] = meminfo.Stack
				fields["mem_locked"] = meminfo.Locked
				fields["mem_swap"] = meminfo.Swap
			}
			io_counters, err := proc.IOCounters()
			if err != nil {
				s.debugf("Error getting IO counters %v: %v", err, p.Pid)
			} else {
				fields["io_counters_read_count"] = io_counters.ReadCount
				fields["io_counters_read_bytes"] = io_counters.ReadBytes
				fields["io_counters_write_count"] = io_counters.WriteCount
				fields["io_counters_write_bytes"] = io_counters.WriteBytes
			}
			num_fds, err := proc.NumFDs()
			if err != nil {
				s.debugf("Error getting num_fds %v: %v", err, p.Pid)
			} else {
				fields["num_fds"] = num_fds
			}
		}
	}

	a.AddFields("slapos",
		fields,
		map[string]string{
			"name":     p.Name,
			"slappart": partitionTagPrefix + p.Group,
		},
	)
}

func init() {
	inputs.Add("slapos", func() telegraf.Input {
		return &SlapOSMetricCollector{}
	})
}
