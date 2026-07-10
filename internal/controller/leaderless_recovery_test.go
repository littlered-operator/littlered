/*
Copyright 2026 The littlered Authors.

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

package controller

import (
	"testing"
	"time"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	redisclient "github.com/littlered-operator/littlered-operator/internal/redis"
)

// --- builders -------------------------------------------------------------

type snSpec struct {
	reachable  bool
	monitoring bool
}

type rnSpec struct {
	ip        string
	reachable bool
	keys      int64
	offset    int64
	replid    string
	role      string
}

func buildState(sentinels []snSpec, redis []rnSpec) *redisclient.SentinelClusterState {
	s := redisclient.NewSentinelClusterState()
	for i, sn := range sentinels {
		ip := "10.0.1." + string(rune('0'+i))
		s.SentinelNodes[ip] = &redisclient.SentinelNodeState{
			IP: ip, Reachable: sn.reachable, Monitoring: sn.monitoring,
		}
	}
	for _, rn := range redis {
		s.ValidIPs[rn.ip] = true
		s.RedisNodes[rn.ip] = &redisclient.RedisNodeState{
			IP: rn.ip, PodName: "pod-" + rn.ip, Reachable: rn.reachable,
			Keys: rn.keys, Offset: rn.offset, Replid: rn.replid, Role: rn.role,
		}
	}
	return s
}

// three bare (reachable, not monitoring) sentinels — the deadlock signature.
func bareQuorum() []snSpec {
	return []snSpec{{reachable: true}, {reachable: true}, {reachable: true}}
}

// --- planLeaderlessRecovery: every gate and tier --------------------------

func TestPlanLeaderlessRecovery(t *testing.T) {
	const cooldown = 30 * time.Second
	now := time.Unix(1_000_000, 0)
	elapsed := func() *time.Time { t := now.Add(-cooldown - time.Second); return &t }
	fresh := func() *time.Time { t := now.Add(-5 * time.Second); return &t }

	tests := []struct {
		name         string
		sentinels    []snSpec
		redis        []rnSpec
		allowUnsafe  bool
		bootstrapIP  string
		since        *time.Time
		wantAction   recoveryAction
		wantMasterIP string
		wantDiverged bool
		wantHolders  int
	}{
		// --- GATES: nothing must happen -----------------------------------
		{
			name:       "gate: a sentinel is monitoring -> not a deadlock",
			sentinels:  []snSpec{{reachable: true, monitoring: true}, {reachable: true}, {reachable: true}},
			since:      elapsed(),
			wantAction: recoveryClearMarker,
		},
		{
			name:       "gate: bare but below quorum -> clear",
			sentinels:  []snSpec{{reachable: true}, {reachable: false}, {reachable: false}},
			since:      elapsed(),
			wantAction: recoveryClearMarker,
		},
		{
			name:       "gate: deadlock but no marker yet -> start cooldown",
			sentinels:  bareQuorum(),
			since:      nil,
			wantAction: recoveryStartCooldown,
		},
		{
			name:       "gate: within cooldown -> wait (even with a data holder present)",
			sentinels:  bareQuorum(),
			redis:      []rnSpec{{ip: "10.0.0.1", reachable: true, keys: 5, role: "slave"}},
			since:      fresh(),
			wantAction: recoveryWait,
		},
		{
			name:        "gate: cooldown elapsed, 0 holders, no candidate IP -> wait",
			sentinels:   bareQuorum(),
			bootstrapIP: "",
			since:       elapsed(),
			wantAction:  recoveryWait,
		},
		{
			name:        "gate: >=2 holders, opt-in OFF -> refuse (no elect)",
			sentinels:   bareQuorum(),
			redis:       []rnSpec{{ip: "10.0.0.1", reachable: true, keys: 5, role: "slave"}, {ip: "10.0.0.2", reachable: true, keys: 9, role: "slave"}},
			allowUnsafe: false,
			since:       elapsed(),
			wantAction:  recoveryRefuse,
			wantHolders: 2,
		},

		// --- FUNCTIONALITY: the right thing must happen -------------------
		{
			name:         "0 holders + candidate -> seed redis-0",
			sentinels:    bareQuorum(),
			redis:        []rnSpec{{ip: "10.0.0.1", reachable: false}, {ip: "10.0.0.2", reachable: false}},
			bootstrapIP:  "10.0.0.1",
			since:        elapsed(),
			wantAction:   recoverySeedNoData,
			wantMasterIP: "10.0.0.1",
		},
		{
			name:         "1 holder -> promote it, no opt-in needed",
			sentinels:    bareQuorum(),
			redis:        []rnSpec{{ip: "10.0.0.1", reachable: true, keys: 42, role: "slave"}, {ip: "10.0.0.2", reachable: true, keys: 0}, {ip: "10.0.0.3", reachable: false}},
			allowUnsafe:  false, // deliberately off: single holder must still recover
			bootstrapIP:  "10.0.0.2",
			since:        elapsed(),
			wantAction:   recoveryPromoteSurvivor,
			wantMasterIP: "10.0.0.1",
			wantHolders:  1,
		},
		{
			name:         ">=2 holders + opt-in ON -> elect highest offset",
			sentinels:    bareQuorum(),
			redis:        []rnSpec{{ip: "10.0.0.1", reachable: true, keys: 500, offset: 100, replid: "A", role: "slave"}, {ip: "10.0.0.2", reachable: true, keys: 10, offset: 900, replid: "A", role: "slave"}},
			allowUnsafe:  true,
			since:        elapsed(),
			wantAction:   recoveryUnsafeElect,
			wantMasterIP: "10.0.0.2",
			wantDiverged: false,
			wantHolders:  2,
		},
		{
			name:         ">=2 holders divergent + opt-in ON -> elect + diverged flag",
			sentinels:    bareQuorum(),
			redis:        []rnSpec{{ip: "10.0.0.1", reachable: true, keys: 10, offset: 100, replid: "A", role: "slave"}, {ip: "10.0.0.2", reachable: true, keys: 10, offset: 200, replid: "B", role: "slave"}},
			allowUnsafe:  true,
			since:        elapsed(),
			wantAction:   recoveryUnsafeElect,
			wantMasterIP: "10.0.0.2",
			wantDiverged: true,
			wantHolders:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := buildState(tt.sentinels, tt.redis)
			got := planLeaderlessRecovery(state, 2, tt.allowUnsafe, tt.bootstrapIP, tt.since, now, cooldown)
			if got.action != tt.wantAction {
				t.Fatalf("action = %v, want %v", got.action, tt.wantAction)
			}
			if tt.wantMasterIP != "" && got.masterIP != tt.wantMasterIP {
				t.Errorf("masterIP = %q, want %q", got.masterIP, tt.wantMasterIP)
			}
			if got.diverged != tt.wantDiverged {
				t.Errorf("diverged = %v, want %v", got.diverged, tt.wantDiverged)
			}
			if tt.wantHolders != 0 && got.holders != tt.wantHolders {
				t.Errorf("holders = %d, want %d", got.holders, tt.wantHolders)
			}
		})
	}
}

// --- needsPromotion -------------------------------------------------------

func TestNeedsPromotion(t *testing.T) {
	tests := []struct {
		name string
		node *redisclient.RedisNodeState
		want bool
	}{
		{"reachable replica -> promote", &redisclient.RedisNodeState{IP: "1.1.1.1", Reachable: true, Role: "slave"}, true},
		{"reachable master -> no", &redisclient.RedisNodeState{IP: "1.1.1.1", Reachable: true, Role: RoleMaster}, false},
		{"unreachable -> no (starts fresh via startup script)", &redisclient.RedisNodeState{IP: "1.1.1.1", Reachable: false, Role: "slave"}, false},
		{"absent -> no", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := redisclient.NewSentinelClusterState()
			if tt.node != nil {
				s.RedisNodes["1.1.1.1"] = tt.node
			}
			if got := needsPromotion(s, "1.1.1.1"); got != tt.want {
				t.Errorf("needsPromotion() = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- pickBootstrapMasterIP ------------------------------------------------

func TestPickBootstrapMasterIP(t *testing.T) {
	lr := &littleredv1alpha1.LittleRed{}
	lr.Name = "store"
	r := &LittleRedReconciler{}

	tests := []struct {
		name     string
		redisMap map[string]string
		want     string
	}{
		{
			name:     "prefers redis-0",
			redisMap: map[string]string{"10.0.0.2": "store-redis-1", "10.0.0.1": "store-redis-0", "10.0.0.3": "store-redis-2"},
			want:     "10.0.0.1",
		},
		{
			name:     "redis-0 absent falls back to lowest-ordinal name",
			redisMap: map[string]string{"10.0.0.3": "store-redis-2", "10.0.0.2": "store-redis-1"},
			want:     "10.0.0.2",
		},
		{
			name:     "no pods yields empty",
			redisMap: map[string]string{},
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.pickBootstrapMasterIP(lr, tt.redisMap); got != tt.want {
				t.Errorf("pickBootstrapMasterIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
