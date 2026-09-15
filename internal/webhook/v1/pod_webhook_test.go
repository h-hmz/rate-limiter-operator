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
	"strings"

	. "github.com/onsi/ginkgo/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	. "github.com/onsi/gomega"
)

var _ = Describe("Pod Webhook", func() {

	var defaulter PodCustomDefaulter

	DescribeTable("leaves pods that did not opt in untouched",
		func(annotations map[string]string) {
			pod := annotatedPod("untouched")
			pod.Annotations = annotations
			before := pod.DeepCopy()

			Expect(defaulter.Default(ctx, pod)).To(Succeed())
			Expect(pod).To(Equal(before))
		},
		Entry("no annotations", nil),
		Entry("inject is false", map[string]string{injectAnnotation: "false", appPortAnnotation: "80"}),
	)

	DescribeTable("denies an opted-in pod whose app port is unusable",
		func(annotations map[string]string) {
			pod := annotatedPod("denied")
			pod.Annotations = annotations

			Expect(defaulter.Default(ctx, pod)).NotTo(Succeed())
		},
		Entry("missing", map[string]string{injectAnnotation: "true"}),
		Entry("not a number", map[string]string{injectAnnotation: "true", appPortAnnotation: "http"}),
		Entry("zero", map[string]string{injectAnnotation: "true", appPortAnnotation: "0"}),
		Entry("above 65535", map[string]string{injectAnnotation: "true", appPortAnnotation: "65536"}),
		Entry("the proxy listen port", map[string]string{injectAnnotation: "true", appPortAnnotation: "15001"}),
		Entry("the proxy metrics port", map[string]string{injectAnnotation: "true", appPortAnnotation: "15090"}),
	)

	It("appends rl-iptables-init, then rl-proxy, after the pod's own init containers", func() {
		pod := annotatedPod("injected")
		pod.Spec.InitContainers = []corev1.Container{{Name: "migrate", Image: "busybox:1.37"}}

		Expect(defaulter.Default(ctx, pod)).To(Succeed())

		iptables := pod.Spec.InitContainers[1]
		Expect(iptables.RestartPolicy).To(BeNil())
		Expect(strings.Join(iptables.Command, " ")).To(ContainSubstring("--dport 80 -j REDIRECT --to-port 15001"))

		proxy := pod.Spec.InitContainers[2]
		Expect(proxy.RestartPolicy).To(HaveValue(Equal(corev1.ContainerRestartPolicyAlways)))
		Expect(proxy.Env).To(ContainElement(corev1.EnvVar{Name: "RL_APP_PORT", Value: "80"}))

	})

	It("does not inject twice", func() {
		pod := annotatedPod("twice")
		Expect(defaulter.Default(ctx, pod)).To(Succeed())
		once := pod.DeepCopy()

		Expect(defaulter.Default(ctx, pod)).To(Succeed())
		Expect(pod).To(Equal(once))
	})

	DescribeTable("puts the paths of HTTP probes on the app port into RL_BYPASS_PATHS",
		func(setProbes func(app *corev1.Container), want string) {
			pod := annotatedPod("probes")
			setProbes(&pod.Spec.Containers[0])

			Expect(defaulter.Default(ctx, pod)).To(Succeed())

			proxy := pod.Spec.InitContainers[1]
			if want == "" {
				// HaveField matches on the Name alone, whatever the value is.
				Expect(proxy.Env).NotTo(ContainElement(HaveField("Name", "RL_BYPASS_PATHS")))
				return
			}
			Expect(proxy.Env).To(ContainElement(corev1.EnvVar{Name: "RL_BYPASS_PATHS", Value: want}))
		},
		Entry("no probes", func(*corev1.Container) {}, ""),
		Entry("numeric port", func(c *corev1.Container) {
			c.ReadinessProbe = httpProbe("/ready", intstr.FromInt32(80))
		}, "/ready"),
		Entry("named port", func(c *corev1.Container) {
			c.LivenessProbe = httpProbe("/live", intstr.FromString("http"))
		}, "/live"),
		// The kubelet resolves a named port against the probe's own container only.
		Entry("named port the container does not declare", func(c *corev1.Container) {
			c.LivenessProbe = httpProbe("/live", intstr.FromString("admin"))
		}, ""),
		Entry("a different port from the annotated one", func(c *corev1.Container) {
			c.ReadinessProbe = httpProbe("/ready", intstr.FromInt32(9090))
		}, ""),
		Entry("no leading slash", func(c *corev1.Container) {
			c.ReadinessProbe = httpProbe("healthz", intstr.FromInt32(80))
		}, "/healthz"),
		// The proxy matches on the path alone, so the query must be dropped.
		Entry("query string", func(c *corev1.Container) {
			c.ReadinessProbe = httpProbe("/healthz?verbose=1", intstr.FromInt32(80))
		}, "/healthz"),
		Entry("empty path", func(c *corev1.Container) {
			c.ReadinessProbe = httpProbe("", intstr.FromInt32(80))
		}, "/"),
		// probeBypassPaths visits probes in the order: startup --> readiness --> liveness.
		Entry("repeated path, in probe order", func(c *corev1.Container) {
			c.StartupProbe = httpProbe("/startup", intstr.FromInt32(80))
			c.ReadinessProbe = httpProbe("/healthz", intstr.FromInt32(80))
			c.LivenessProbe = httpProbe("/healthz", intstr.FromString("http"))
		}, "/startup,/healthz"),
		Entry("TCP probe on the app port", func(c *corev1.Container) {
			c.ReadinessProbe = tcpProbe(intstr.FromInt32(80))
		}, ""),
	)
})

// annotatedPod returns a pod in "default" that opts in to injection with app
// port 80. Its only container, "app", declares 80 as a port named "http".
func annotatedPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				injectAnnotation:  "true",
				appPortAnnotation: "80",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "app",
				Image: "nginx:1.29-alpine",
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 80}},
			}},
		},
	}
}

func httpProbe(path string, port intstr.IntOrString) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: path, Port: port},
		},
	}
}

func tcpProbe(port intstr.IntOrString) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			TCPSocket: &corev1.TCPSocketAction{Port: port},
		},
	}
}
