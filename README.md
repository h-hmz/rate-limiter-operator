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

Annotate the pod template, naming a [policy](#rate-limit-policies) that exists in the pod's namespace:

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
        rl.hhmz.dev/policy: web-limit
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
| `rl.hhmz.dev/policy` | The `RateLimitPolicy` in the pod's own namespace that configures the sidecar. Required. A missing annotation, a policy that does not exist, or a `redis` policy whose store is not provisioned yet all make the API server reject the pod. |

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

`Ready` follows the Redis Deployment's available replica, and is `True` immediately for a memory policy, which has nothing to wait for. A `redis` policy also publishes the coordinates it provisioned under `status.redis`, which is what the webhook reads when it configures a sidecar.

### Requirements and caveats

- **cert-manager** must be installed. It issues the webhook's serving certificate.
- **Injected namespaces need the `privileged` Pod Security level.** The iptables init container adds `NET_ADMIN`, which both `baseline` and `restricted` forbid. The proxy container itself runs as non-root with all capabilities dropped.
- **Clients are keyed by `X-Forwarded-For`.** Injected pods must sit behind an ingress that overwrites that header with the client's address. Requests without it get `400`. Do not expose injected pods directly.
- **The app must serve plain HTTP on the app port.** The proxy forwards over HTTP to `127.0.0.1`, so an app serving TLS, or listening only on the pod IP, will not work.
- **Admission fails open.** If the webhook is unreachable, pods are created without the sidecar instead of being rejected. The cluster keeps working, but that traffic is not limited.
- **Provisioned Redis keeps nothing on disk.** It runs as one replica with snapshots and the append-only log disabled, and rolls with the `Recreate` strategy, so a Redis restart resets every bucket it held. That is a deliberate trade for rate-limiter state, not a durable datastore.
- **Editing a policy does not reach running pods.** The sidecar's configuration is written into the pod at admission, so a changed `requests`, `burst`, `window`, `key` or `failOpen` applies to new pods only.

## Getting Started

### Prerequisites

- Go v1.26.0+, to build from source.
- A container engine: Docker, or Podman with `CONTAINER_TOOL=podman` on the `make` targets below.
- kubectl v1.29+.
- A Kubernetes **v1.29+** cluster. The injected proxy is an init container with `restartPolicy: Always`, and the `SidecarContainers` feature gate that allows it has been on by default since v1.29.
- **cert-manager** in the cluster, as noted above.

### Deploy

```sh
make docker-build docker-push IMG=<registry>/rate-limiter-operator:tag
make deploy IMG=<registry>/rate-limiter-operator:tag
```

`make deploy` renders `config/default`, which includes the CRDs, so it installs them as well. `make install` installs only the CRDs, without the manager.

Example on k3s with podman:

```sh
make docker-build CONTAINER_TOOL=podman IMG=localhost/rate-limiter-operator:dev
podman save localhost/rate-limiter-operator:dev | sudo k3s ctr images import -
make deploy IMG=localhost/rate-limiter-operator:dev
```

### Example

```sh
kubectl apply -k config/samples/                        # a fixedwindow/memory and a tokenbucket/redis policy
kubectl get rlp                                         # READY goes True once Redis reports available
kubectl apply -f config/samples/sample_app_redis.yaml   # nginx, two replicas, behind the redis policy
```

Because the two replicas share one Redis bucket, two requests to one of them exhaust it and the next request to the *other* replica is already limited:

```sh
IPS=($(kubectl get pods -l app=sample-app-redis -o jsonpath='{.items[*].status.podIP}'))
for i in 1 2; do curl -s -o /dev/null -w "%{http_code} " -H "X-Forwarded-For: 203.0.113.7" http://${IPS[0]}/; done
curl -s -o /dev/null -w "%{http_code}\n" -H "X-Forwarded-For: 203.0.113.7" http://${IPS[1]}/
```

`200 200` then `429`. With `store: memory` the second replica answers `200`, because it keeps its own buckets.

### Uninstall

```sh
kubectl delete -f config/samples/sample_app_redis.yaml
kubectl delete -k config/samples/
make undeploy    # the manager, its RBAC and the webhook configuration
make uninstall   # the CRDs, and with them every policy and the Redis each one provisioned
```

## Distribution

`make build-installer IMG=<registry>/rate-limiter-operator:tag` renders every manifest into `dist/install.yaml`, a single file that installs the operator with one `kubectl apply -f` and no clone. It is not committed here, so build it from the tag you want to ship.

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