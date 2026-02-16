package types

import (
	corev1 "k8s.io/api/core/v1"
)

// ClusterContext provides all metadata needed to interact with a Redis/Sentinel cluster,
// regardless of whether it is managed by the LittleRed operator.
type ClusterContext struct {
	Name      string
	Namespace string
	Mode      string // sentinel, cluster

	// Pods grouped by their role/component
	RedisPods    []corev1.Pod
	SentinelPods []corev1.Pod // In sidecar mode, this might be the same as RedisPods

	// Container names to use for 'exec'
	RedisContainer    string
	SentinelContainer string
}
