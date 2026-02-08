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

package v1alpha1

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Default values
const (
	DefaultMode            = "standalone"
	DefaultRegistry        = "docker.io"
	DefaultImagePath       = "valkey/valkey"
	DefaultImageTag        = "8.0"
	DefaultPullPolicy      = corev1.PullIfNotPresent
	DefaultMaxmemoryPolicy = "allkeys-lru"
	DefaultTimeout         = 0
	DefaultTCPKeepalive    = 300
	DefaultServiceType     = corev1.ServiceTypeClusterIP
	DefaultUpdateStrategy  = "RollingUpdate"
	DefaultExporterPath    = "oliver006/redis_exporter"
	DefaultExporterTag     = "v1.66.0"
	DefaultScrapeInterval  = "30s"
	DefaultScrapeTimeout   = "10s"
	DefaultSentinelQuorum  = 2
	DefaultDownAfterMs     = 30000
	DefaultFailoverTimeout = 180000
	DefaultParallelSyncs   = 1
	DefaultSecurityUserID  = int64(999)
	DefaultSecurityGroupID = int64(999)
	RedisPort              = 6379
	RedisExporterPort      = 9121
	SentinelPort           = 26379

	// Requeue defaults
	DefaultFastRequeueInterval        = 2 * time.Second
	DefaultSteadyStateRequeueInterval = 30 * time.Second

	// Cluster defaults
	DefaultClusterShards      = 3
	DefaultReplicasPerShard   = 1
	DefaultClusterNodeTimeout = 15000
	ClusterBusPortOffset      = 10000
	ClusterBusPort            = RedisPort + ClusterBusPortOffset // 16379
)

// SetDefaults applies default values to the LittleRed spec
func (r *LittleRed) SetDefaults() {
	spec := &r.Spec

	// Mode
	if spec.Mode == "" {
		spec.Mode = DefaultMode
	}

	// Image
	spec.Image.SetDefaults()

	// Resources
	setDefaultResources(&spec.Resources)

	// Config
	spec.Config.SetDefaults()

	// Metrics
	spec.Metrics.SetDefaults(spec.Image.Registry)

	// Service
	if spec.Service.Type == "" {
		spec.Service.Type = DefaultServiceType
	}

	// UpdateStrategy
	if spec.UpdateStrategy.Type == "" {
		spec.UpdateStrategy.Type = DefaultUpdateStrategy
	}

	// PodTemplate security context
	if spec.PodTemplate.SecurityContext == nil {
		spec.PodTemplate.SecurityContext = defaultPodSecurityContext()
	}

	// Sentinel defaults (only if sentinel mode)
	if spec.Mode == "sentinel" && spec.Sentinel == nil {
		spec.Sentinel = &SentinelSpec{}
	}
	if spec.Sentinel != nil {
		spec.Sentinel.SetDefaults()
	}

	// Cluster defaults (only if cluster mode)
	if spec.Mode == "cluster" && spec.Cluster == nil {
		spec.Cluster = &ClusterSpec{}
	}
	if spec.Cluster != nil {
		spec.Cluster.SetDefaults()
	}
}

// SetDefaults applies default values to ImageSpec
func (i *ImageSpec) SetDefaults() {
	if i.Registry == "" {
		i.Registry = DefaultRegistry
	}
	if i.Path == "" {
		i.Path = DefaultImagePath
	}
	if i.Tag == "" {
		i.Tag = DefaultImageTag
	}
	if i.PullPolicy == "" {
		i.PullPolicy = DefaultPullPolicy
	}
}

// SetDefaults applies default values to ConfigSpec
func (c *ConfigSpec) SetDefaults() {
	if c.MaxmemoryPolicy == "" {
		c.MaxmemoryPolicy = DefaultMaxmemoryPolicy
	}
	if c.TCPKeepalive == 0 {
		c.TCPKeepalive = DefaultTCPKeepalive
	}
}

// SetDefaults applies default values to MetricsSpec
func (m *MetricsSpec) SetDefaults(mainRegistry string) {
	m.Exporter.SetDefaults(mainRegistry)

	if m.ServiceMonitor.Interval == "" {
		m.ServiceMonitor.Interval = DefaultScrapeInterval
	}
	if m.ServiceMonitor.ScrapeTimeout == "" {
		m.ServiceMonitor.ScrapeTimeout = DefaultScrapeTimeout
	}
}

// SetDefaults applies default values to ExporterSpec
func (e *ExporterSpec) SetDefaults(mainRegistry string) {
	if e.Registry == "" {
		e.Registry = mainRegistry
		if e.Registry == "" {
			e.Registry = DefaultRegistry
		}
	}
	if e.Path == "" {
		e.Path = DefaultExporterPath
	}
	if e.Tag == "" {
		e.Tag = DefaultExporterTag
	}
	setDefaultExporterResources(&e.Resources)
}

// SetDefaults applies default values to SentinelSpec
func (s *SentinelSpec) SetDefaults() {
	if s.Quorum == 0 {
		s.Quorum = DefaultSentinelQuorum
	}
	if s.DownAfterMilliseconds == 0 {
		s.DownAfterMilliseconds = DefaultDownAfterMs
	}
	if s.FailoverTimeout == 0 {
		s.FailoverTimeout = DefaultFailoverTimeout
	}
	if s.ParallelSyncs == 0 {
		s.ParallelSyncs = DefaultParallelSyncs
	}
	setDefaultSentinelResources(&s.Resources)
}

// SetDefaults applies default values to ClusterSpec
func (c *ClusterSpec) SetDefaults() {
	if c.Shards == 0 {
		c.Shards = DefaultClusterShards
	}
	if c.ReplicasPerShard == 0 {
		c.ReplicasPerShard = DefaultReplicasPerShard
	}
	if c.ClusterNodeTimeout == 0 {
		c.ClusterNodeTimeout = DefaultClusterNodeTimeout
	}
}

// GetTotalNodes returns the total number of cluster nodes (shards * (1 + replicas))
func (c *ClusterSpec) GetTotalNodes() int {
	return c.Shards * (1 + c.ReplicasPerShard)
}

func setDefaultResources(r *corev1.ResourceRequirements) {
	if r.Requests == nil {
		r.Requests = corev1.ResourceList{}
	}
	if r.Limits == nil {
		r.Limits = corev1.ResourceList{}
	}

	if _, ok := r.Requests[corev1.ResourceCPU]; !ok {
		r.Requests[corev1.ResourceCPU] = DefaultCPU
	}
	if _, ok := r.Requests[corev1.ResourceMemory]; !ok {
		r.Requests[corev1.ResourceMemory] = DefaultMemory
	}
	if _, ok := r.Limits[corev1.ResourceCPU]; !ok {
		r.Limits[corev1.ResourceCPU] = DefaultCPU
	}
	if _, ok := r.Limits[corev1.ResourceMemory]; !ok {
		r.Limits[corev1.ResourceMemory] = DefaultMemory
	}
}

func setDefaultExporterResources(r *corev1.ResourceRequirements) {
	if r.Requests == nil {
		r.Requests = corev1.ResourceList{}
	}
	if r.Limits == nil {
		r.Limits = corev1.ResourceList{}
	}

	if _, ok := r.Requests[corev1.ResourceCPU]; !ok {
		r.Requests[corev1.ResourceCPU] = DefaultExporterCPURequest
	}
	if _, ok := r.Requests[corev1.ResourceMemory]; !ok {
		r.Requests[corev1.ResourceMemory] = DefaultExporterMemoryRequest
	}
	if _, ok := r.Limits[corev1.ResourceCPU]; !ok {
		r.Limits[corev1.ResourceCPU] = DefaultExporterCPULimit
	}
	if _, ok := r.Limits[corev1.ResourceMemory]; !ok {
		r.Limits[corev1.ResourceMemory] = DefaultExporterMemoryLimit
	}
}

func setDefaultSentinelResources(r *corev1.ResourceRequirements) {
	if r.Requests == nil {
		r.Requests = corev1.ResourceList{}
	}
	if r.Limits == nil {
		r.Limits = corev1.ResourceList{}
	}

	if _, ok := r.Requests[corev1.ResourceCPU]; !ok {
		r.Requests[corev1.ResourceCPU] = DefaultSentinelCPU
	}
	if _, ok := r.Requests[corev1.ResourceMemory]; !ok {
		r.Requests[corev1.ResourceMemory] = DefaultSentinelMemory
	}
	if _, ok := r.Limits[corev1.ResourceCPU]; !ok {
		r.Limits[corev1.ResourceCPU] = DefaultSentinelCPU
	}
	if _, ok := r.Limits[corev1.ResourceMemory]; !ok {
		r.Limits[corev1.ResourceMemory] = DefaultSentinelMemory
	}
}

func defaultPodSecurityContext() *corev1.PodSecurityContext {
	runAsNonRoot := true
	return &corev1.PodSecurityContext{
		RunAsNonRoot: &runAsNonRoot,
		RunAsUser:    ptr(DefaultSecurityUserID),
		RunAsGroup:   ptr(DefaultSecurityGroupID),
		FSGroup:      ptr(DefaultSecurityGroupID),
	}
}

func ptr[T any](v T) *T {
	return &v
}

// CalculateMaxmemory calculates maxmemory based on memory limit (90% of limit)
func (r *LittleRed) CalculateMaxmemory() string {
	if r.Spec.Config.Maxmemory != "" {
		// Try to parse as Kubernetes quantity (e.g., "200Mi", "1Gi")
		if qty, err := resource.ParseQuantity(r.Spec.Config.Maxmemory); err == nil {
			return fmt.Sprintf("%d", qty.Value())
		}
		// If not a valid quantity, return as-is (might be raw bytes)
		return r.Spec.Config.Maxmemory
	}

	memLimit := r.Spec.Resources.Limits[corev1.ResourceMemory]
	if memLimit.IsZero() {
		memLimit = DefaultMemory
	}

	// Calculate 90% of memory limit
	bytes := memLimit.Value()
	maxmemoryBytes := int64(float64(bytes) * 0.9)

	return fmt.Sprintf("%d", maxmemoryBytes)
}

// GetEffectiveMaxmemoryPolicy returns the maxmemory policy, defaulting to allkeys-lru
func (r *LittleRed) GetEffectiveMaxmemoryPolicy() string {
	if r.Spec.Config.MaxmemoryPolicy != "" {
		return r.Spec.Config.MaxmemoryPolicy
	}
	return DefaultMaxmemoryPolicy
}

// GetPort returns the Redis port (with TLS awareness for future use)
func (r *LittleRed) GetPort() int32 {
	return RedisPort
}

// GetExporterPort returns the metrics exporter port
func (r *LittleRed) GetExporterPort() int32 {
	return RedisExporterPort
}

// GetRequeueIntervals returns the effective requeue intervals
func (r *LittleRed) GetRequeueIntervals() (fast, steady time.Duration) {
	fast = DefaultFastRequeueInterval
	steady = DefaultSteadyStateRequeueInterval

	if r.Spec.RequeueIntervals != nil {
		if r.Spec.RequeueIntervals.Fast != nil {
			fast = r.Spec.RequeueIntervals.Fast.Duration
		}
		if r.Spec.RequeueIntervals.SteadyState != nil {
			steady = r.Spec.RequeueIntervals.SteadyState.Duration
		}
	}
	return
}

// ParseMaxmemory parses the maxmemory string into bytes
func ParseMaxmemory(maxmemory string) (int64, error) {
	q, err := resource.ParseQuantity(maxmemory)
	if err != nil {
		return 0, err
	}
	return q.Value(), nil
}
