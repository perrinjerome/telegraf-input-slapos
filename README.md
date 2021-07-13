A telegraf input plugin to collect metrics from processes running in a slapos node.

This uses supervisor to get running services and collect RAM, CPU and files metrics from these processes.

## Installation

```bash
go get -v github.com/perrinjerome/telegraf-input-slapos/...
```

## Usage

Example config (`plugin.conf`):

```toml
[[inputs.slapos]]
## Folder where partitions are located
instance_root = "/srv/slapgrid/"

## filepath.Glob pattern to look for recursive instances
recursive_instance_glob_pattern = "*/srv/runner/inst*"

## Path of supervisor socket, relative to instance root
socket_name = "sv.sock"

## Ignore processes whose name match these regular expressions
ignored_process_names = [
    "^certificate_authority.*",
    "^crond-.*",
    "^monitor-.*",
    "^bootstrap-monitor.*",
    "^watchdog$",
]
```

Usage in telegraf:

```toml
[[inputs.execd]]
command = ["telegraf-input-slapos", "-config", "/path/to/telegraf-input-slapos/plugin.conf"]
## if needed to access other partitions, it should be possible to slapos-format so that the
## partition running telegraf is member of other partitions. Another approach is to use sudo:
# command = ["sudo", "telegraf-input-slapos", "-config", "/path/to/telegraf-input-slapos/plugin.conf"]
```
