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

package v1

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	rlv1alpha1 "github.com/h-hmz/rate-limiter-operator/api/rl/v1alpha1"
)

const (
	// Opt-in. Duplicated in the CEL matchCondition in config/webhook/patches/pod_webhook_gating.yaml.
	injectAnnotation = "rl.hhmz.dev/inject"

	// The port the app listens on, and so the port PREROUTING captures.
	appPortAnnotation = "rl.hhmz.dev/app-port"

	// The RateLimitPolicy the sidecar is configured from, read from the pod's own namespace.
	policyAnnotation = "rl.hhmz.dev/policy"

	initContainerName = "rl-iptables-init"
	sidecarName       = "rl-proxy"

	iptablesImage = "registry.k8s.io/build-image/distroless-iptables:v0.9.6"
	proxyImage    = "ghcr.io/h-hmz/rate-limiter-proxy:0.1.0"

	proxyListenPort  = 15001
	proxyMetricsPort = 15090

	// The distroless "nonroot" uid baked into the proxy image.
	proxyUserID = 65532
)

// nolint:unused
// log is for logging in this package.
var podlog = logf.Log.WithName("pod-resource")

// SetupPodWebhookWithManager registers the webhook for Pod in the manager.
func SetupPodWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(&PodCustomDefaulter{Reader: mgr.GetAPIReader()}).
		WithDefaulterCustomPath("/mutate-core-v1-pod").
		Complete()
}

// +kubebuilder:webhook:path=/mutate-core-v1-pod,mutating=true,failurePolicy=Ignore,sideEffects=None,groups=core,resources=pods,verbs=create,versions=v1,name=mpod-v1.kb.io,admissionReviewVersions=v1

// PodCustomDefaulter struct is responsible for setting default values on the custom resource of the
// Kind Pod when those are created or updated.
type PodCustomDefaulter struct {
	// Reader for fetching the RateLimitPolicy named by the pod.
	Reader client.Reader
}

// Default implements webhook.CustomDefaulter so a webhook will be registered for the Kind Pod.
func (d *PodCustomDefaulter) Default(ctx context.Context, pod *corev1.Pod) error {
	log := podlog.WithValues("namespace", pod.Namespace, "pod", podDisplayName(pod))

	// Skip pods that are not targeted by this webhook.
	if pod.Annotations[injectAnnotation] != "true" {
		return nil
	}

	// Idempotency gate.
	if hasContainer(pod.Spec.InitContainers, sidecarName) ||
		hasContainer(pod.Spec.Containers, sidecarName) {
		log.V(1).Info("sidecar already present, leaving pod unchanged")
		return nil
	}

	appPort, err := resolveAppPort(pod)
	if err != nil {
		return err
	}

	policy, err := d.resolvePolicy(ctx, pod)
	if err != nil {
		return err
	}

	// Collect the probe paths that must skip the limiter.
	bypassPaths := probeBypassPaths(pod, appPort, log)

	// Mutate.
	pod.Spec.InitContainers = append(pod.Spec.InitContainers,
		iptablesInitContainer(appPort), // Order matters: the nat rule must exist before anything listens.
		proxySidecar(appPort, policy, bypassPaths),
	)

	log.Info("injected sidecar",
		"appPort", appPort,
		"policy", policy.Name,
		"algorithm", policy.Spec.Algorithm,
		"store", policy.Spec.Store,
		"bypassPaths", bypassPaths,
	)

	return nil
}

// resolvePolicy reads the RateLimitPolicy named by the pod's policy annotation.
// A missing annotation, an unknown policy, or a redis-store policy whose coordinates are not published yet all deny the pod.
func (d *PodCustomDefaulter) resolvePolicy(ctx context.Context, pod *corev1.Pod) (*rlv1alpha1.RateLimitPolicy, error) {
	name := pod.Annotations[policyAnnotation]
	if name == "" {
		return nil, fmt.Errorf("required annotation %q is missing", policyAnnotation)
	}

	var policy rlv1alpha1.RateLimitPolicy
	key := client.ObjectKey{Name: name, Namespace: pod.Namespace}

	if err := d.Reader.Get(ctx, key, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf(
				"annotation %q names RateLimitPolicy %q, which does not exist in namespace %q",
				policyAnnotation,
				name,
				pod.Namespace,
			)
		}
		return nil, fmt.Errorf("reading RateLimitPolicy %q: %w", name, err)
	}

	if policy.Spec.Store == rlv1alpha1.StoreRedis && policy.Status.Redis == nil {
		return nil, fmt.Errorf("RateLimitPolicy %q uses the redis store but has not published status.redis yet", name)
	}

	return &policy, nil
}

// resolveAppPort reads the app port from the pod's annotations.
func resolveAppPort(pod *corev1.Pod) (int32, error) {
	appPortStr, ok := pod.Annotations[appPortAnnotation]
	if !ok {
		return 0, fmt.Errorf("required annotation %q is missing", appPortAnnotation)
	}

	p, err := strconv.Atoi(appPortStr)
	if err != nil {
		return 0, fmt.Errorf(
			"annotation %q must be a valid port, got %q",
			appPortAnnotation,
			appPortStr,
		)
	}

	if p < 1 || p > 65535 {
		return 0, fmt.Errorf(
			"annotation %q must be between 1 and 65535, got %d",
			appPortAnnotation,
			p,
		)
	}

	if p == proxyListenPort || p == proxyMetricsPort {
		return 0, fmt.Errorf(
			"app port must not conflict with proxy ports %d or %d",
			proxyListenPort,
			proxyMetricsPort,
		)
	}

	return int32(p), nil
}

// probeBypassPaths returns the exact paths to put in RL_BYPASS_PATHS,
// checked against HTTPGet probes (Startup, Readiness, Liveness).
func probeBypassPaths(pod *corev1.Pod, appPort int32, log logr.Logger) []string {

	var bypassPaths []string

	for _, c := range pod.Spec.Containers {

		plog := log.WithValues("container", c.Name)

		for _, p := range []*corev1.Probe{c.StartupProbe, c.ReadinessProbe, c.LivenessProbe} {

			if p == nil {
				continue
			}

			switch {
			case p.HTTPGet != nil:
				port, ok := resolveProbePort(&c, p.HTTPGet.Port)
				if !ok || port != appPort {
					continue
				}

				u, err := url.Parse(p.HTTPGet.Path)
				if err != nil {
					plog.Error(err, "Could not derive bypass path, probe will not be bypassed", "path", p.HTTPGet.Path)
					continue
				}

				// Adds leading slash as kubelet does in case it is missing
				if !strings.HasPrefix(u.Path, "/") {
					u.Path = "/" + u.Path
				}

				if !slices.Contains(bypassPaths, u.Path) {
					if u.Path == "/" {
						plog.Info("Probe targets the root path, so requests to / will skip the limiter")
					}
					bypassPaths = append(bypassPaths, u.Path)
				}

			case p.TCPSocket != nil:
				if port, ok := resolveProbePort(&c, p.TCPSocket.Port); ok && port == appPort {
					plog.Info("TCP probe on the app port cannot be bypassed and tests the proxy instead of the app", "port", port)
				}

			case p.GRPC != nil:
				if p.GRPC.Port == appPort {
					plog.Info("gRPC probe on the app port cannot be bypassed", "port", p.GRPC.Port)
				}
			}
		}
	}
	return bypassPaths
}

// resolveProbePort returns the port number a probe targets.
func resolveProbePort(c *corev1.Container, port intstr.IntOrString) (int32, bool) {
	if port.Type == intstr.Int {
		return port.IntVal, true
	}
	for _, cp := range c.Ports {
		if cp.Name == port.StrVal {
			return cp.ContainerPort, true
		}
	}
	return 0, false
}

// iptablesInitContainer returns the regular init container that installs the
// pod-local traffic interception rule.
//
// The returned container must be added to PodSpec.InitContainers. Because it is
// a regular init container—not a sidecar-style init container with
// RestartPolicy: Always—the kubelet waits for it to exit successfully before
// starting any application containers, including the proxy.
//
// If this command fails, the application and proxy are not started. Whether the
// kubelet retries the init container is governed by the Pod's restart policy.
//
// The command installs this rule:
//
//	iptables-nft -t nat -A PREROUTING \
//	    -p tcp --dport <appPort> \
//	    -j REDIRECT --to-port <proxyListenPort>
//
// Because containers in a Pod share one network namespace, the rule installed
// by this init container is later used by both the application container and
// the proxy container.
//
// The rule intercepts new inbound TCP connections addressed to appPort and
// changes their destination port to proxyListenPort. The destination address
// remains local to the Pod, so routing delivers the connection to the proxy.
//
// iptables-nft is invoked explicitly instead of using the generic "iptables"
// command. In this image, generic "iptables" initially points to a wrapper
// script. That wrapper:
//
//  1. inspects the existing x_tables and nf_tables rules;
//  2. chooses either iptables-legacy or iptables-nft;
//  3. attempts to rewrite generic symlinks under /usr/sbin to point to the
//     selected implementation; and
//  4. executes the requested command through that implementation.
//
// Backend detection is unnecessary and potentially unreliable here because a
// newly created Pod network namespace may contain no existing rules from which
// to infer a backend. The wrapper's symlink writes are also incompatible with
// a read-only root filesystem.
//
// Calling iptables-nft directly bypasses the wrapper and deterministically
// installs the rule in the kernel's nf_tables backend. Unlike
// iptables-legacy, it also does not require access to /run/xtables.lock.
// Consequently, the container does not need a writable root filesystem.
//
// Finally, the node's kernel must provide the nf_tables and NAT functionality
// required by iptables-nft and the REDIRECT target.
func iptablesInitContainer(appPort int32) corev1.Container {
	return corev1.Container{
		Name:  initContainerName,
		Image: iptablesImage,

		// Execute iptables-nft:
		//
		// -t nat
		//     Selects the NAT table.
		//
		// -A PREROUTING
		//     Appends the rule to the PREROUTING chain. Packets arriving through
		//     the Pod's network interface encounter this chain before routing
		//     decides that their destination is local.
		//
		// -p tcp --dport <appPort>
		//     Restricts interception to TCP connections whose original
		//     destination port is the application's listening port.
		//
		// -j REDIRECT --to-port <proxyListenPort>
		//     Performs destination NAT to the local Pod and changes the
		//     destination port to the proxy's listening port.
		Command: []string{
			"iptables-nft",
			"-t", "nat",
			"-A", "PREROUTING",
			"-p", "tcp",
			"--dport", strconv.Itoa(int(appPort)),
			"-j", "REDIRECT",
			"--to-port", strconv.Itoa(proxyListenPort),
		},

		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				// Start with no Linux capabilities, then add back only the
				// capability required to modify Netfilter state.
				//
				// NET_RAW is intentionally omitted: installing an nftables rule
				// requires NET_ADMIN, not the ability to create raw sockets.
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"NET_ADMIN"},
			},
			Privileged: new(false),

			// UID 0 does NOT make the container fully privileged. Its effective
			// kernel permissions remain constrained by the capability set above.
			RunAsNonRoot:             new(false),
			RunAsUser:                new(int64(0)),
			AllowPrivilegeEscalation: new(false),

			// The command only reads binaries and configuration from the image.
			// Its actual change is sent to the kernel's nf_tables subsystem.
			//
			// This remains possible because:
			//   - iptables-nft is invoked directly, so no wrapper rewrites
			//     symlinks under /usr/sbin; and
			//   - iptables-nft does not need /run/xtables.lock.
			ReadOnlyRootFilesystem: new(true),
		},

		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
		},
	}
}

// proxySidecar builds the native sidecar: an init container whose restartPolicy
// is Always, so the kubelet starts it before the app and stops it after.
func proxySidecar(appPort int32, policy *rlv1alpha1.RateLimitPolicy, bypassPaths []string) corev1.Container {
	always := corev1.ContainerRestartPolicyAlways

	env := []corev1.EnvVar{
		{Name: "RL_APP_PORT", Value: strconv.Itoa(int(appPort))},
		{Name: "RL_LISTEN_PORT", Value: strconv.Itoa(proxyListenPort)},
		{Name: "RL_METRICS_PORT", Value: strconv.Itoa(proxyMetricsPort)},
		{Name: "RL_ALGORITHM", Value: string(policy.Spec.Algorithm)},
		{Name: "RL_STORE", Value: string(policy.Spec.Store)},
	}

	if policy.Spec.Key != "" {
		env = append(env, corev1.EnvVar{Name: "RL_KEY", Value: string(policy.Spec.Key)})
	}
	if policy.Spec.KeyHeader != "" {
		env = append(env, corev1.EnvVar{Name: "RL_KEY_HEADER", Value: policy.Spec.KeyHeader})
	}
	if policy.Spec.FailOpen != nil {
		env = append(env, corev1.EnvVar{
			Name:  "RL_FAIL_OPEN",
			Value: strconv.FormatBool(*policy.Spec.FailOpen),
		})
	}

	switch policy.Spec.Algorithm {
	case rlv1alpha1.AlgorithmFixedWindow:
		env = append(env,
			corev1.EnvVar{Name: "RL_RATE", Value: strconv.Itoa(int(policy.Spec.FixedWindow.Requests))},
			corev1.EnvVar{Name: "RL_WINDOW", Value: policy.Spec.FixedWindow.Window.Duration.String()},
		)
	case rlv1alpha1.AlgorithmTokenBucket:
		requests := float64(policy.Spec.TokenBucket.Requests)
		period := policy.Spec.TokenBucket.Period
		rate := strconv.FormatFloat(requests/period.Duration.Seconds(), 'f', -1, 64)
		env = append(env,
			corev1.EnvVar{Name: "RL_RATE", Value: rate},
			corev1.EnvVar{Name: "RL_BURST", Value: strconv.Itoa(int(policy.Spec.TokenBucket.Burst))},
		)
	}

	if policy.Spec.Store == rlv1alpha1.StoreRedis {
		env = append(env,
			corev1.EnvVar{Name: "RL_REDIS_ADDR", Value: policy.Status.Redis.Address},
			corev1.EnvVar{Name: "RL_REDIS_PASSWORD", ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: policy.Status.Redis.PasswordSecretName,
					},
					Key: policy.Status.Redis.PasswordSecretKey,
				}},
			},
		)
	}

	if len(bypassPaths) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "RL_BYPASS_PATHS",
			Value: strings.Join(bypassPaths, ","),
		})
	}

	return corev1.Container{
		Name:          sidecarName,
		Image:         proxyImage,
		RestartPolicy: &always,
		Env:           env,
		Ports: []corev1.ContainerPort{
			{Name: "rl-proxy", ContainerPort: proxyListenPort},
			{Name: "rl-metrics", ContainerPort: proxyMetricsPort},
		},
		// The admin listener has its own port, which the PREROUTING rule does
		// not match, so this probe is neither redirected nor rate limited and
		// needs no bypass entry.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz",
					Port: intstr.FromInt32(proxyMetricsPort),
				},
			},
			PeriodSeconds: 5,
		},
		SecurityContext: &corev1.SecurityContext{
			RunAsNonRoot:             new(true),
			RunAsUser:                new(int64(proxyUserID)),
			AllowPrivilegeEscalation: new(false),
			ReadOnlyRootFilesystem:   new(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("25m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
			// No CPU limit since every request to the app now passes
			// through this container, so CFS throttling would surface as
			// latency on the app's own traffic.
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
		},
	}
}

func hasContainer(containers []corev1.Container, name string) bool {
	for i := range containers {
		if containers[i].Name == name {
			return true
		}
	}
	return false
}

// podDisplayName returns something loggable.
//
// On CREATE, a pod produced by a Deployment has an EMPTY metadata.name. The
// name is expanded from generateName by the API server after admission runs, so
// logging pod.Name here prints nothing at all for exactly the pods this webhook
// exists to handle.
func podDisplayName(pod *corev1.Pod) string {
	if pod.Name != "" {
		return pod.Name
	}
	if pod.GenerateName != "" {
		return pod.GenerateName + "<pending>"
	}
	return "<unnamed>"
}
