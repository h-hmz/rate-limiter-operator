# rate-limiter-operator

A kubernetes Operator that injects a rate-limiting sidecar in front of HTTP applications requiring it.

## Description

The operator runs a mutating admission webhook on Pods. When a pod opts in, the webhook injects [rate-limiter-proxy](https://github.com/h-hmz/rate-limiter-proxy) as a sidecar and redirects the pod's inbound traffic through it. The proxy is built on the [rate-limiter](https://github.com/h-hmz/rate-limiter) library. The app keeps its own port and its own configuration, and never learns that it is being proxied.

### How it works

Each injected pod gets two init containers. Its app containers are left untouched.

1. **`rl-iptables-init`** runs once, before anything else starts, and installs a single rule in the pod's network namespace:

   ```
   iptables-nft -t nat -A PREROUTING -p tcp --dport <appPort> -j REDIRECT --to-port 15001
   ```

2. **`rl-proxy`** runs as a native sidecar, an init container with `restartPolicy: Always`, so it starts before the app and stops after it. It listens on port 15001 and forwards to `127.0.0.1:<appPort>`. It serves `/metrics` and `/healthz` on port 15090, which the rule does not capture.

```
client -> ingress -> podIP:appPort
                         |   PREROUTING REDIRECT
                         v
                    rl-proxy :15001
                         |   dial 127.0.0.1:appPort
                         v
                      app :appPort
```

There is no redirect loop, even though the rule also matches the proxy's own connection to the app. That connection starts inside the pod, so the kernel makes its NAT decision in the `OUTPUT` hook, which has no rule. When its packets come back in over loopback and pass `PREROUTING`, the connection already has a NAT decision, so the rule is skipped. Only connections that arrive from outside the pod are redirected.

The injected images are `ghcr.io/h-hmz/rate-limiter-proxy:0.1.0` and `registry.k8s.io/build-image/distroless-iptables:v0.9.6`.

### Usage

Annotate the pod template:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  selector:
    matchLabels:
      app: web
  template:
    metadata:
      labels:
        app: web
      annotations:
        rl.hhmz.dev/inject: "true"
        rl.hhmz.dev/app-port: "8080"
    spec:
      containers:
        - name: web
          image: registry.example.com/web:1.0
          ports:
            - name: http
              containerPort: 8080
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
```

| Annotation | Meaning |
| --- | --- |
| `rl.hhmz.dev/inject` | Must be exactly `"true"`. Any other value, or no annotation, leaves the pod alone. |
| `rl.hhmz.dev/app-port` | The port the app listens on. Required. A missing or invalid value, or one equal to 15001 or 15090, makes the API server reject the pod. |

Injection happens only when a pod is created, so restart existing workloads to pick up the sidecar:

```sh
kubectl rollout restart deployment/web
```

Pods in `rate-limiter-operator-system`, `kube-system` and `cert-manager` are never injected.

### Health probes

Kubelet probes dial the pod IP, so they are captured like any other request, and they carry no `X-Forwarded-For`. The webhook therefore reads every HTTP probe aimed at the app port and adds its path to the proxy's bypass list. Those probes skip the limiter and still reach the app. Nothing needs configuring.

A probe on `/`, including an HTTP probe with no path, turns the limiter off for the root path, which is usually the app's main traffic.

TCP and gRPC probes on the app port cannot be bypassed. A TCP probe connects to the proxy and passes without testing the app, and a gRPC probe fails. Use HTTP probes on the app port, or probe a different port.

### Current limits

Every injected pod gets the same fixed limit: a token bucket of 2 requests per second with a burst of 2, counted per client IP. Counters live in each pod's memory, so every replica enforces the limit separately.

Per-workload limits and a shared Redis store are planned. They will come from a `RateLimitPolicy` resource named by a pod annotation.

### Requirements and caveats

- **cert-manager** must be installed. It issues the webhook's serving certificate.
- **Injected namespaces need the `privileged` Pod Security level.** The iptables init container adds `NET_ADMIN`, which both `baseline` and `restricted` forbid. The proxy container itself runs as non-root with all capabilities dropped.
- **Clients are keyed by `X-Forwarded-For`.** Injected pods must sit behind an ingress that overwrites that header with the client's address. Requests without it get `400`. Do not expose injected pods directly.
- **The app must serve plain HTTP on the app port.** The proxy forwards over HTTP to `127.0.0.1`, so an app serving TLS, or listening only on the pod IP, will not work.
- **Admission fails open.** If the webhook is unreachable, pods are created without the sidecar instead of being rejected. The cluster keeps working, but that traffic is not limited.

## Getting Started

### Prerequisites
- go version v1.24.6+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/rate-limiter-operator:tag
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/rate-limiter-operator:tag
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Project Distribution

Following the options to release and provide this solution to the users.

### By providing a bundle with all YAML files

1. Build the installer for the image built and published in the registry:

```sh
make build-installer IMG=<some-registry>/rate-limiter-operator:tag
```

**NOTE:** The makefile target mentioned above generates an 'install.yaml'
file in the dist directory. This file contains all the resources built
with Kustomize, which are necessary to install this project without its
dependencies.

2. Using the installer

Users can just run 'kubectl apply -f <URL for YAML BUNDLE>' to install
the project, i.e.:

```sh
kubectl apply -f https://raw.githubusercontent.com/<org>/rate-limiter-operator/<tag or branch>/dist/install.yaml
```

### By providing a Helm Chart

1. Build the chart using the optional helm plugin

```sh
kubebuilder edit --plugins=helm/v2-alpha
```

2. See that a chart was generated under 'dist/chart', and users
can obtain this solution from there.

**NOTE:** If you change the project, you need to update the Helm Chart
using the same command above to sync the latest changes. Furthermore,
if you create webhooks, you need to use the above command with
the '--force' flag and manually ensure that any custom configuration
previously added to 'dist/chart/values.yaml' or 'dist/chart/manager/manager.yaml'
is manually re-applied afterwards.

## License

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

