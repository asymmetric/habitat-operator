# habitat-operator

Installs [habitat-operator](https://github.com/kinvolk/habitat-operator) to create/configure/manage Habitat services atop Kubernetes.

## TL;DR;

```console
$ helm repo add habitat https://urltochart.org/
$ helm install habitat/habitat-operator
```

## Introduction

This chart bootstraps a [habitat-operator](https://github.com/kinvolk/habitat-operator) deployment on a [Kubernetes](http://kubernetes.io) cluster using the [Helm](https://helm.sh) package manager.

## Prerequisites
  - Habitat `>= 0.36.0`
  - Kubernetes cluster with version `1.7.x`, `1.8.x` or `1.9.x`.

### RBAC
If role-based access control (RBAC) is enabled in your cluster, you may need to give Tiller (the server-side component of Helm) additional permissions. **If RBAC is not enabled, be sure to set `rbacEnable` to `false` when installing the chart.**

1. Create a ServiceAccount for Tiller in the `kube-system` namespace
```console
$ kubectl -n kube-system create sa tiller
```

2. Create a ClusterRoleBinding for Tiller
```console
$ cat <<EOF | kubectl create -f -
apiVersion: rbac.authorization.k8s.io/v1alpha1
kind: ClusterRoleBinding
metadata:
  name: tiller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: tiller
    namespace: kube-system
EOF

$ kubectl create clusterrolebinding tiller --clusterrole cluster-admin --serviceaccount=kube-system:tiller
```

3. Install Tiller, specifying the new ServiceAccount
```console
$ helm init --service-account tiller
```

## Installing the Chart

To install the chart with the release name `my-release`:

```console
$ helm install --name my-release habitat/habitat-operator
```

The command deploys habitat-operator on the Kubernetes cluster in the default configuration. The [configuration](#configuration) section lists the parameters that can be configured during installation.

## Uninstalling the Chart

To uninstall/delete the `my-release` deployment:

```console
$ helm delete my-release
```

The command removes all the Kubernetes components associated with the chart and deletes the release.

## Configuration

The following tables lists the configurable parameters of the habitat-operator chart and their default values.

Parameter | Description | Default
--- | --- | ---
`image.repository` | Image | `docker.io/kinvolk/habitat-operator`
`image.tag` | Image tag | `v0.4.0`
`image.pullPolicy` | Image pull policy | `IfNotPresent`
`kubeletService.enable` | If true, the operator will create a service for scraping kubelets | `true`
`kubeletService.namespace` | The namespace in which the kubelet service should be created | `kube-system`
`kubeletService.name` | The name of the kubelet service to be created | `kubelet`
`nodeSelector` | Node labels for pod assignment | `{}`
`rbacEnable` | If true, create & use RBAC resources | `true`
`resources` | Pod resource requests & limits | `{}`

Specify each parameter using the `--set key=value[,key=value]` argument to `helm install`. For example,

```console
$ helm install --name my-release habitat/habitat-operator --set sendAnalytics=true
```

Alternatively, a YAML file that specifies the values for the above parameters can be provided while installing the chart. For example,

```console
$ helm install --name my-release habitat/habitat-operator -f values.yaml
```

> **Tip**: You can use the default [values.yaml](values.yaml)
