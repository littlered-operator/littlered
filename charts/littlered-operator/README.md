# LittleRed Operator Helm Chart

Helm chart for deploying the LittleRed operator to Kubernetes.

## Prerequisites

- Kubernetes 1.28+
- Helm 3.x

## Installation

```bash
helm install littlered ./charts/littlered-operator \
  -n littlered-system \
  --create-namespace
```

## Uninstallation

```bash
helm uninstall littlered -n littlered-system
```

Note: CRDs are not deleted automatically. To remove them:

```bash
kubectl delete crd littlereds.chuck-chuck-chuck.net
```

## Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Operator image repository | `ghcr.io/littlered-operator/littlered-operator` |
| `image.tag` | Operator image tag | `0.1.0` |
| `image.pullPolicy` | Image pull policy | `IfNotPresent` |
| `replicas` | Number of operator replicas | `1` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `128Mi` |
| `resources.requests.cpu` | CPU request | `10m` |
| `resources.requests.memory` | Memory request | `64Mi` |
| `serviceAccount.create` | Create ServiceAccount | `true` |
| `serviceAccount.name` | ServiceAccount name (auto-generated if empty) | `""` |
| `nodeSelector` | Node selector for operator pods | `{}` |
| `tolerations` | Tolerations for operator pods | `[]` |
| `affinity` | Affinity rules for operator pods | `{}` |
| `leaderElection.enabled` | Enable leader election | `true` |

## Examples

### Basic Installation

```bash
helm install littlered ./charts/littlered-operator -n littlered-system --create-namespace
```

### Custom Image

```bash
helm install littlered ./charts/littlered-operator \
  -n littlered-system \
  --create-namespace \
  --set image.repository=myregistry/littlered-operator \
  --set image.tag=v0.1.0
```

### With Custom Resources

```bash
helm install littlered ./charts/littlered-operator \
  -n littlered-system \
  --create-namespace \
  --set resources.limits.memory=256Mi \
  --set resources.requests.memory=128Mi
```

### Using a Values File

Create `my-values.yaml`:

```yaml
image:
  repository: myregistry/littlered-operator
  tag: v0.1.0

resources:
  limits:
    cpu: 1
    memory: 256Mi
  requests:
    cpu: 100m
    memory: 128Mi

nodeSelector:
  kubernetes.io/os: linux
```

Install:

```bash
helm install littlered ./charts/littlered-operator \
  -n littlered-system \
  --create-namespace \
  -f my-values.yaml
```

## After Installation

Once the operator is running, create LittleRed resources:

```yaml
apiVersion: chuck-chuck-chuck.net/v1alpha1
kind: LittleRed
metadata:
  name: my-cache
spec:
  mode: standalone  # or sentinel
```

See the [main README](../../README.md) for full LittleRed CR documentation.

## Upgrading

```bash
helm upgrade littlered ./charts/littlered-operator -n littlered-system
```

## RBAC

The chart creates the following RBAC resources:

- **ClusterRole**: Permissions to manage LittleRed CRs, Pods, Services, ConfigMaps, StatefulSets, Secrets, and ServiceMonitors
- **ClusterRoleBinding**: Binds the ClusterRole to the operator ServiceAccount
- **Role** (namespaced): Leader election permissions
- **RoleBinding** (namespaced): Binds the leader election Role
