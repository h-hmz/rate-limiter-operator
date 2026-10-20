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

package rl

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	appsv1ac "k8s.io/client-go/applyconfigurations/apps/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"

	rlv1alpha1 "github.com/h-hmz/rate-limiter-operator/api/rl/v1alpha1"
)

const (
	// ManagedByLabelKey and ManagedByLabelValue mark every object this operator provisions.
	ManagedByLabelKey   = "app.kubernetes.io/managed-by"
	ManagedByLabelValue = "rate-limiter-operator"

	redisImage    = "redis:8.2.10-alpine3.22"
	redisReplicas = 1

	// redisPasswordKey is the Secret key holding the generated password.
	redisPasswordKey = "redis-password"

	// redisPasswordHashAnnotation carries a hash of the current password on the Redis pod
	// template, so rotating the Secret rolls the Deployment.
	redisPasswordHashAnnotation = "rl.hhmz.dev/redis-password-hash"

	// ConditionReady is the single condition type a policy carries.
	ConditionReady = "Ready"

	// FieldOwner names this operator in the managedFields when doing Server-Side Apply.
	FieldOwner = "rate-limiter-operator"

	// ReasonNoProvisioningRequired is a memory-store policy, which has nothing to build.
	ReasonNoProvisioningRequired = "NoProvisioningRequired"
	// ReasonRedisAvailable means the Redis Deployment reports an available replica.
	ReasonRedisAvailable = "RedisAvailable"
	// ReasonRedisUnavailable means the Redis Deployment reports no, not yet or no more available replica.
	ReasonRedisUnavailable = "RedisUnavailable"
	// ReasonProvisionFailed means this pass could not converge the children at all.
	ReasonProvisionFailed = "ProvisionFailed"
)

// RateLimitPolicyReconciler reconciles a RateLimitPolicy object
type RateLimitPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=rl.hhmz.dev,resources=ratelimitpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rl.hhmz.dev,resources=ratelimitpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=rl.hhmz.dev,resources=ratelimitpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/reconcile
func (r *RateLimitPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rlp rlv1alpha1.RateLimitPolicy

	if err := r.Get(ctx, req.NamespacedName, &rlp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	provisionErr := r.provision(ctx, &rlp)

	if err := r.updateStatus(ctx, &rlp, provisionErr); err != nil {
		if apierrors.IsConflict(err) && provisionErr == nil {
			logf.FromContext(ctx).V(1).Info("status write conflicted, requeueing")
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, errors.Join(provisionErr, err)
	}

	// Returning the provisioning error requeues with backoff.
	return ctrl.Result{}, provisionErr
}

// provision brings the policy's children into line with its spec.
// A memory-store policy has no children, so this is a no-op for it.
func (r *RateLimitPolicyReconciler) provision(ctx context.Context, p *rlv1alpha1.RateLimitPolicy) error {
	if p.Spec.Store != rlv1alpha1.StoreRedis {
		return nil
	}
	passwordHash, err := r.reconcileRedisSecret(ctx, p)
	if err != nil {
		return err
	}
	if err := r.reconcileRedisService(ctx, p); err != nil {
		return err
	}
	return r.reconcileRedisDeployment(ctx, p, passwordHash)
}

// reconcileRedisSecret creates the Secret holding the Redis password, and returns a hash
// of the password for the Deployment to carry on its pod template.
//
// Data is immutable, therefore rotating the password means deleting the Secret
// and letting the next reconcile mint a new one.
func (r *RateLimitPolicyReconciler) reconcileRedisSecret(ctx context.Context, p *rlv1alpha1.RateLimitPolicy) (string, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: redisChildName(p), Namespace: p.Namespace},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if len(secret.Data[redisPasswordKey]) == 0 {
			// 26 characters of RFC 4648 base32, at least 128 bits of entropy.
			secret.Data = map[string][]byte{redisPasswordKey: []byte(rand.Text())}
		}

		secret.Type = corev1.SecretTypeOpaque
		secret.Immutable = new(true)

		// Merged, so labels added elsewhere are left in place.
		secret.Labels = mergeLabels(secret.Labels, redisChildLabels(p))

		return controllerutil.SetControllerReference(p, secret, r.Scheme)
	})
	if err != nil {
		return "", fmt.Errorf("reconciling redis secret: %w", err)
	}

	logf.FromContext(ctx).V(1).Info("reconciled secret", "name", secret.Name, "op", op)

	sum := sha256.Sum256(secret.Data[redisPasswordKey])
	return hex.EncodeToString(sum[:]), nil
}

// reconcileRedisService reconciles the ClusterIP Service in front of the Redis pod.
func (r *RateLimitPolicyReconciler) reconcileRedisService(ctx context.Context, p *rlv1alpha1.RateLimitPolicy) error {

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      redisChildName(p),
			Namespace: p.Namespace,
		},
	}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {

		svc.Spec.Selector = redisChildLabels(p)
		svc.Labels = mergeLabels(svc.Labels, redisChildLabels(p))
		svc.Spec.Type = corev1.ServiceTypeClusterIP

		svc.Spec.Ports = []corev1.ServicePort{
			{
				Name:       "redis",
				Port:       6379,
				TargetPort: intstr.FromString("redis"),
				Protocol:   corev1.ProtocolTCP,
			},
		}

		return controllerutil.SetControllerReference(
			p,
			svc,
			r.Scheme,
		)
	})

	if err != nil {
		return err
	}

	logf.FromContext(ctx).V(1).Info("reconciled service", "name", svc.Name, "op", op)

	return nil
}

// redisDeploymentAC describes the Redis Deployment as an apply configuration: only the fields this operator intends to own.
// Everything else, including the API server's defaults, is left untouched instead of being wiped and re-defaulted on every pass.
func redisDeploymentAC(p *rlv1alpha1.RateLimitPolicy, passwordHash string) *appsv1ac.DeploymentApplyConfiguration {
	name := redisChildName(p)
	labels := redisChildLabels(p)

	container := corev1ac.Container().
		WithName("redis").
		WithImage(redisImage).
		WithImagePullPolicy(corev1.PullIfNotPresent).
		WithPorts(corev1ac.ContainerPort().
			WithName("redis").
			WithContainerPort(6379)).
		WithEnv(corev1ac.EnvVar().
			WithName("REDIS_PASSWORD").
			WithValueFrom(corev1ac.EnvVarSource().
				WithSecretKeyRef(corev1ac.SecretKeySelector().
					WithName(name).
					WithKey(redisPasswordKey)))).
		// Disable RDB snapshots and AOF persistence: a rate limiter's buckets are ephemeral.
		WithArgs("--requirepass", "$(REDIS_PASSWORD)", "--save", "", "--appendonly", "no").
		WithReadinessProbe(corev1ac.Probe().
			WithTCPSocket(corev1ac.TCPSocketAction().
				WithPort(intstr.FromString("redis"))).
			WithPeriodSeconds(5))

	return appsv1ac.Deployment(name, p.Namespace).
		WithLabels(labels).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(rlv1alpha1.SchemeGroupVersion.String()).
			WithKind("RateLimitPolicy").
			WithName(p.Name).
			WithUID(p.UID).
			WithController(true).
			WithBlockOwnerDeletion(true)).
		WithSpec(appsv1ac.DeploymentSpec().
			WithReplicas(redisReplicas).
			// Recreate, not RollingUpdate: one replica with no persistence, so a second pod would briefly serve a different, empty set of buckets.
			WithStrategy(appsv1ac.DeploymentStrategy().
				WithType(appsv1.RecreateDeploymentStrategyType)).
			WithSelector(metav1ac.LabelSelector().
				WithMatchLabels(labels)).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(labels).
				// A new password changes this annotation, which rolls the pod.
				WithAnnotations(map[string]string{redisPasswordHashAnnotation: passwordHash}).
				WithSpec(corev1ac.PodSpec().
					WithContainers(container))))
}

// reconcileRedisDeployment reconciles the single-replica Redis Deployment.
func (r *RateLimitPolicyReconciler) reconcileRedisDeployment(ctx context.Context, p *rlv1alpha1.RateLimitPolicy, passwordHash string) error {
	dep := redisDeploymentAC(p, passwordHash)

	if err := r.Apply(ctx, dep, client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return fmt.Errorf("applying redis deployment: %w", err)
	}

	logf.FromContext(ctx).V(1).Info("applied deployment", "name", redisChildName(p))

	return nil
}

// redisChildName is the name shared by every resource provisioned for a policy.
func redisChildName(p *rlv1alpha1.RateLimitPolicy) string {
	return p.Name + "-redis"
}

// redisChildLabels is the label set every provisioned resource carries.
func redisChildLabels(p *rlv1alpha1.RateLimitPolicy) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "redis",
		"app.kubernetes.io/instance":  p.Name,
		"app.kubernetes.io/component": "ratelimit-store",
		ManagedByLabelKey:             ManagedByLabelValue,
	}
}

// mergeLabels merges labels so a label another tool put on the object survives a reconcile.
func mergeLabels(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	maps.Copy(dst, src)
	return dst
}

// redisStatus is the coordinates of the Redis provisioned for this policy. The webhook reads
// them from status rather than recomputing the child-naming convention.
func redisStatus(p *rlv1alpha1.RateLimitPolicy) *rlv1alpha1.RedisStatus {
	return &rlv1alpha1.RedisStatus{
		Address:            fmt.Sprintf("%s.%s.svc:6379", redisChildName(p), p.Namespace),
		PasswordSecretName: redisChildName(p),
		PasswordSecretKey:  redisPasswordKey,
	}
}

// updateStatus records the policy's current state, and writes only when something changed.
func (r *RateLimitPolicyReconciler) updateStatus(ctx context.Context, rlp *rlv1alpha1.RateLimitPolicy, provisionErr error) error {
	cond, err := r.readyCondition(ctx, rlp, provisionErr)
	if err != nil {
		return err
	}
	cond.ObservedGeneration = rlp.Generation

	changed := rlp.Status.ObservedGeneration != rlp.Generation
	rlp.Status.ObservedGeneration = rlp.Generation

	if meta.SetStatusCondition(&rlp.Status.Conditions, cond) {
		changed = true
	}

	// redisStatus is computed only after a successful provision. A later failure does not invalidate it.
	if provisionErr == nil && rlp.Spec.Store == rlv1alpha1.StoreRedis {
		desired := redisStatus(rlp)
		if rlp.Status.Redis == nil || *rlp.Status.Redis != *desired {
			rlp.Status.Redis = desired
			changed = true
		}
	}

	if !changed {
		return nil
	}
	return r.Status().Update(ctx, rlp)
}

// readyCondition describes the readiness of the provisioned ressources.
func (r *RateLimitPolicyReconciler) readyCondition(ctx context.Context, p *rlv1alpha1.RateLimitPolicy, provisionErr error) (metav1.Condition, error) {
	if provisionErr != nil {
		return metav1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonProvisionFailed,
			Message: provisionErr.Error(),
		}, nil
	}

	if p.Spec.Store != rlv1alpha1.StoreRedis {
		return metav1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionTrue,
			Reason:  ReasonNoProvisioningRequired,
			Message: "memory store needs no provisioned resources",
		}, nil
	}

	var dep appsv1.Deployment
	key := client.ObjectKey{Name: redisChildName(p), Namespace: p.Namespace}

	switch err := r.Get(ctx, key, &dep); {
	case err == nil:
	case apierrors.IsNotFound(err):
		// Either it was only just created and the cache has not caught up or it was removed;
		// from here both read the same and both resolve themselves.
		return metav1.Condition{
			Type:    ConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  ReasonRedisUnavailable,
			Message: "redis deployment does not exist yet",
		}, nil
	default:
		return metav1.Condition{}, fmt.Errorf("reading redis deployment: %w", err)
	}

	desired := int32(redisReplicas)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}

	cond := metav1.Condition{
		Type:    ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  ReasonRedisUnavailable,
		Message: fmt.Sprintf("%d/%d redis replicas available", dep.Status.AvailableReplicas, desired),
	}
	if dep.Status.AvailableReplicas >= desired {
		cond.Status = metav1.ConditionTrue
		cond.Reason = ReasonRedisAvailable
	}
	return cond, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *RateLimitPolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Spec changes only: filter out status writes that do not bump metadata.Generation. Doesn't apply to children's statuses.
		For(&rlv1alpha1.RateLimitPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Named("rl-ratelimitpolicy").
		Complete(r)
}
