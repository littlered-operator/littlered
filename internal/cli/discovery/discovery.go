package discovery

import (
	"context"
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	littleredv1alpha1 "github.com/littlered-operator/littlered-operator/api/v1alpha1"
	"github.com/littlered-operator/littlered-operator/internal/cli/types"
)

// GetContext builds a ClusterContext either from a CR or via heuristics
func GetContext(ctx context.Context, k8sClient client.Client, namespace, name, mode string, unmanaged bool) (*types.ClusterContext, error) {
	if !unmanaged {
		return getFromCR(ctx, k8sClient, namespace, name)
	}
	return discoverUnmanaged(ctx, k8sClient, namespace, name, mode)
}

func getFromCR(ctx context.Context, k8sClient client.Client, namespace, name string) (*types.ClusterContext, error) {
	lr := &littleredv1alpha1.LittleRed{}
	err := k8sClient.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, lr)
	if err != nil {
		return nil, fmt.Errorf("failed to get LittleRed CR: %w", err)
	}

	podList := &corev1.PodList{}
	err = k8sClient.List(ctx, podList, client.InNamespace(namespace), client.MatchingLabels{
		"app.kubernetes.io/instance": name,
	})
	if err != nil {
		return nil, err
	}

	cCtx := &types.ClusterContext{
		Name:              name,
		Namespace:         namespace,
		Mode:              string(lr.Spec.Mode),
		RedisContainer:    "redis",
		SentinelContainer: "sentinel",
	}

	for _, pod := range podList.Items {
		comp := pod.Labels["app.kubernetes.io/component"]
		if comp == "redis" || comp == "cluster" {
			cCtx.RedisPods = append(cCtx.RedisPods, pod)
		}
		if comp == "sentinel" {
			cCtx.SentinelPods = append(cCtx.SentinelPods, pod)
		}
	}

	return cCtx, nil
}

func discoverUnmanaged(ctx context.Context, k8sClient client.Client, namespace, name, mode string) (*types.ClusterContext, error) {
	cCtx := &types.ClusterContext{
		Name:              name,
		Namespace:         namespace,
		Mode:              mode,
		RedisContainer:    "redis",
		SentinelContainer: "sentinel",
	}

	podList := &corev1.PodList{}
	err := k8sClient.List(ctx, podList, client.InNamespace(namespace))
	if err != nil {
		return nil, err
	}

	// Define heuristics as regex patterns
	// 1. CloudPirates: <name>-redis-<digit>
	cpRegex := regexp.MustCompile(fmt.Sprintf("^%s-redis-[0-9]+$", regexp.QuoteMeta(name)))
	// 2. Bitnami: <name>-redis-node-<digit>
	bitnamiRegex := regexp.MustCompile(fmt.Sprintf("^%s-redis-node-[0-9]+$", regexp.QuoteMeta(name)))
	// 3. Generic Cluster: <name>-cluster-[0-9]+
	clusterRegex := regexp.MustCompile(fmt.Sprintf("^%s-cluster-[0-9]+$", regexp.QuoteMeta(name)))
	// 4. Spotahome Redis: rfr-<name>-<digit>
	shRedisRegex := regexp.MustCompile(fmt.Sprintf("^rfr-%s-[0-9]+$", regexp.QuoteMeta(name)))
	// 5. Spotahome Sentinel: rfs-<name>-...
	shSentinelRegex := regexp.MustCompile(fmt.Sprintf("^rfs-%s-[a-z0-9]+(-[a-z0-9]+)?$", regexp.QuoteMeta(name)))

	for _, pod := range podList.Items {
		isRedis := cpRegex.MatchString(pod.Name) || bitnamiRegex.MatchString(pod.Name) ||
			clusterRegex.MatchString(pod.Name) || shRedisRegex.MatchString(pod.Name)

		isSentinel := shSentinelRegex.MatchString(pod.Name)

		if isRedis {
			cCtx.RedisPods = append(cCtx.RedisPods, pod)
			// In sidecar mode (CP/Bitnami), the sentinel is in the same pod
			if !isSentinel {
				for _, container := range pod.Spec.Containers {
					if container.Name == "sentinel" {
						cCtx.SentinelPods = append(cCtx.SentinelPods, pod)
						break
					}
				}
			}
		}

		if isSentinel {
			cCtx.SentinelPods = append(cCtx.SentinelPods, pod)
		}
	}

	if len(cCtx.RedisPods) == 0 && len(cCtx.SentinelPods) == 0 {
		return nil, fmt.Errorf("could not find any pods matching CloudPirates, Bitnami, Cluster, or Spotahome patterns for %q in namespace %q", name, namespace)
	}

	return cCtx, nil
}
