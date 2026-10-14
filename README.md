# rate-limiter-operator

A kubernetes Operator that injects a rate-limiting sidecar in front of HTTP applications requiring it.

## Description

The operator runs a mutating admission webhook on Pods. When a pod opts in, the webhook injects [rate-limiter-proxy](https://github.com/h-hmz/rate-limiter-proxy) as a sidecar and redirects the pod's inbound traffic through it. The proxy is built on the [rate-limiter](https://github.com/h-hmz/rate-limiter) library. The app keeps its own port and its own configuration, and never learns that it is being proxied.

Independently of the webhook, the operator reconciles `RateLimitPolicy` resources, provisioning a Redis store for each policy that asks for one: see [Rate limit policies](#rate-limit-policies).

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

### Rate limit policies

A `RateLimitPolicy` describes a rate limiter configuration. The operator provisions the backing store the policy asks for and reports its readiness.

```yaml
apiVersion: rl.hhmz.dev/v1alpha1
kind: RateLimitPolicy
metadata:
  name: web-limit
spec:
  algorithm: tokenbucket   # or fixedwindow
  store: redis             # or memory
  tokenBucket:
    requests: 100          # tokens added per period
    period: 1s             # optional, defaults to 1s
    burst: 20              # bucket capacity
```

A `fixedwindow` policy carries the other parameter block:

```yaml
spec:
  algorithm: fixedwindow
  store: memory
  fixedWindow:
    requests: 100
    window: 1m
```

| Field | Meaning |
| --- | --- |
| `algorithm` | `tokenbucket` or `fixedwindow`. Names which parameter block must be present; supplying the other one, both, or neither is rejected. |
| `tokenBucket.requests`, `.period` | The sustained rate: `requests` tokens added per `period`. `period` defaults to `1s`, so one request per `10s` is a tenth of a request per second. |
| `tokenBucket.burst` | Bucket capacity: the most requests servable back to back after an idle stretch. |
| `fixedWindow.requests`, `.window` | Requests allowed per window. Every window starts empty, so a client may spend its whole allowance the moment one rolls over. |
| `store` | `memory` gives every pod its own buckets, so the limit is enforced per replica. `redis` shares one set of buckets across every pod using the policy. Immutable once created. |
| `key` | `ip` (default) buckets by the client address from `X-Forwarded-For`. `apikey` buckets by a header instead. |
| `keyHeader` | The header carrying the API key, valid only with `key: apikey`. Defaults to `X-API-Key`. |
| `failOpen` | `true` (default) turns a store outage into allows, `false` into `500`s. A store unreachable at startup is always a refusal to start. |

Durations are Go durations, so `30s`, `1m`, `500ms` and `1h30m` all work.

Policies are namespaced, and a policy belongs in the namespace of the pods that will use it.

#### What a redis policy provisions

For `store: redis` the operator creates three objects named `<policy>-redis` in the policy's namespace:

- a single-replica **Deployment** running `redis:8.2.10-alpine3.22`, with `--requirepass` read from the Secret,
- a ClusterIP **Service** on port 6379,
- an immutable **Secret** holding a generated password.

All three carry an owner reference to the policy, so deleting the policy deletes them. The Redis pod template carries a hash of the password, which is how rotation works: delete the Secret, and the next reconcile mints a new password and rolls the pod.

`store: memory` provisions nothing.

```sh
$ kubectl get rlp
NAME        ALGORITHM     STORE    READY   AGE
web-limit   tokenbucket   redis    True    30s
```

`Ready` follows the Redis Deployment's available replica, and is `True` immediately for a memory policy, which has nothing to wait for.

### Requirements and caveats

- **cert-manager** must be installed. It issues the webhook's serving certificate.
- **Injected namespaces need the `privileged` Pod Security level.** The iptables init container adds `NET_ADMIN`, which both `baseline` and `restricted` forbid. The proxy container itself runs as non-root with all capabilities dropped.
- **Clients are keyed by `X-Forwarded-For`.** Injected pods must sit behind an ingress that overwrites that header with the client's address. Requests without it get `400`. Do not expose injected pods directly.
- **The app must serve plain HTTP on the app port.** The proxy forwards over HTTP to `127.0.0.1`, so an app serving TLS, or listening only on the pod IP, will not work.
- **Admission fails open.** If the webhook is unreachable, pods are created without the sidecar instead of being rejected. The cluster keeps working, but that traffic is not limited.
- **Provisioned Redis keeps nothing on disk.** It runs as one replica with snapshots and the append-only log disabled, and rolls with the `Recreate` strategy, so a Redis restart resets every bucket it held. That is a deliberate trade for rate-limiter state, not a durable datastore.

## Getting Started

### Prerequisites
- go version v1.26.0+.
- docker version 17.03+.
- kubectl version v1.29+.
- Access to a Kubernetes **v1.29+** cluster. The injected proxy is an init container with `restartPolicy: Always`, and the `SidecarContainers` feature gate that allows it has been on by default since v1.29.

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

**Create a RateLimitPolicy**
You can apply the sample from `config/samples`:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: The sample is a memory-store policy, so it provisions nothing and goes `Ready` at once. Set `store: redis` to watch the Deployment, Service and Secret appear.

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

