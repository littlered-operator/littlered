/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package redis

import (
	"testing"
)

func TestParseInfoField(t *testing.T) {
	// Sample Redis INFO replication output
	sampleInfo := `# Replication
role:master
connected_slaves:2
slave0:ip=10.244.0.5,port=6379,state=online,offset=1234,lag=0
slave1:ip=10.244.0.6,port=6379,state=online,offset=1234,lag=1
master_failover_state:no-failover
master_replid:8371445ee47dc3f56fbe54c1e3c27a4e6b3d3b5d
master_replid2:0000000000000000000000000000000000000000
master_repl_offset:1234
second_repl_offset:-1
repl_backlog_active:1
repl_backlog_size:1048576
repl_backlog_first_byte_offset:1
repl_backlog_histlen:1234
`

	sampleReplicaInfo := `# Replication
role:slave
master_host:10.244.0.4
master_port:6379
master_link_status:up
master_last_io_seconds_ago:0
master_sync_in_progress:0
slave_read_repl_offset:1234
slave_repl_offset:1234
slave_priority:100
slave_read_only:1
replica_announced:1
connected_slaves:0
master_failover_state:no-failover
master_replid:8371445ee47dc3f56fbe54c1e3c27a4e6b3d3b5d
master_replid2:0000000000000000000000000000000000000000
master_repl_offset:1234
second_repl_offset:-1
repl_backlog_active:1
repl_backlog_size:1048576
repl_backlog_first_byte_offset:1
repl_backlog_histlen:1234
`

	tests := []struct {
		name     string
		info     string
		field    string
		expected string
	}{
		{
			name:     "parse role master",
			info:     sampleInfo,
			field:    "role",
			expected: "master",
		},
		{
			name:     "parse role slave",
			info:     sampleReplicaInfo,
			field:    "role",
			expected: "slave",
		},
		{
			name:     "parse master_host",
			info:     sampleReplicaInfo,
			field:    "master_host",
			expected: "10.244.0.4",
		},
		{
			name:     "parse master_port",
			info:     sampleReplicaInfo,
			field:    "master_port",
			expected: "6379",
		},
		{
			name:     "parse connected_slaves",
			info:     sampleInfo,
			field:    "connected_slaves",
			expected: "2",
		},
		{
			name:     "parse master_repl_offset",
			info:     sampleInfo,
			field:    "master_repl_offset",
			expected: "1234",
		},
		{
			name:     "field not found",
			info:     sampleInfo,
			field:    "nonexistent_field",
			expected: "",
		},
		{
			name:     "empty info",
			info:     "",
			field:    "role",
			expected: "",
		},
		{
			name:     "field at start of line",
			info:     "role:master\nother:value",
			field:    "role",
			expected: "master",
		},
		{
			name:     "field with similar prefix",
			info:     "master_host:10.0.0.1\nmaster_host_port:1234",
			field:    "master_host",
			expected: "10.0.0.1",
		},
		{
			name:     "windows line endings",
			info:     "role:master\r\nconnected_slaves:1\r\n",
			field:    "role",
			expected: "master",
		},
		{
			name:     "value with special characters",
			info:     "master_replid:8371445ee47dc3f56fbe54c1e3c27a4e6b3d3b5d\n",
			field:    "master_replid",
			expected: "8371445ee47dc3f56fbe54c1e3c27a4e6b3d3b5d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ParseInfoField(tt.info, tt.field)
			if result != tt.expected {
				t.Errorf("ParseInfoField(%q) = %q, want %q", tt.field, result, tt.expected)
			}
		})
	}
}

func TestNewSentinelClient(t *testing.T) {
	addresses := []string{"sentinel-0:26379", "sentinel-1:26379", "sentinel-2:26379"}
	password := "secret"

	client := NewSentinelClient(addresses, password, false)

	if client == nil {
		t.Fatal("NewSentinelClient returned nil")
	}

	if len(client.addresses) != 3 {
		t.Errorf("expected 3 addresses, got %d", len(client.addresses))
	}

	if client.password != password {
		t.Errorf("password = %q, want %q", client.password, password)
	}
}

func TestMasterInfo(t *testing.T) {
	info := &MasterInfo{
		IP:   "10.244.0.5",
		Port: "6379",
	}

	if info.IP != "10.244.0.5" {
		t.Errorf("IP = %q, want %q", info.IP, "10.244.0.5")
	}

	if info.Port != "6379" {
		t.Errorf("Port = %q, want %q", info.Port, "6379")
	}
}
