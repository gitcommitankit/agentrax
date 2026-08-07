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

// Package quota implements tenant quota arithmetic and admission-time in-flight
// reservation used by the validating webhook and TenantQuota reconciler.
package quota

import (
	"fmt"
	"math"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"

	agentraxv1alpha1 "github.com/gitcommitankit/agentrax/api/v1alpha1"
)

// DefaultGPUResourceName is the Kubernetes resource name used to count GPU
// units when --gpu-resource-name is not overridden by the operator flag.
const DefaultGPUResourceName = "nvidia.com/gpu"

// reservationEntry holds in-flight resource counts that have been pre-reserved
// during admission but not yet committed to etcd.
type reservationEntry struct {
	agents   int32
	gpus     int32
	replicas int32
	expiry   time.Time
}

// Enforcer provides quota arithmetic and manages an in-flight reservation map
// to prevent concurrent near-limit creates from both slipping past the quota
// ceiling. The zero value is not usable; use NewEnforcer.
type Enforcer struct {
	gpuResourceName string

	// mu guards reservations.
	mu           sync.Mutex
	reservations map[string]*reservationEntry // keyed by "namespace/adName"

	// done is closed by Stop() to terminate the background sweep goroutine.
	// stopOnce ensures close(done) is called exactly once.
	done     chan struct{}
	stopOnce sync.Once

	// nowFn is overridden in tests to control time.
	nowFn func() time.Time
}

// NewEnforcer creates a new Enforcer. gpuResourceName is the Kubernetes resource
// name used to count GPU units (e.g. "nvidia.com/gpu"). Pass DefaultGPUResourceName
// when the operator --gpu-resource-name flag is not overridden.
func NewEnforcer(gpuResourceName string) *Enforcer {
	if gpuResourceName == "" {
		gpuResourceName = DefaultGPUResourceName
	}
	e := &Enforcer{
		gpuResourceName: gpuResourceName,
		reservations:    make(map[string]*reservationEntry),
		done:            make(chan struct{}),
		nowFn:           time.Now,
	}
	// Start the background sweep goroutine to purge expired reservations.
	go e.sweepLoop()
	return e
}

// Stop terminates the background sweep goroutine. It is idempotent; repeated
// calls are safe and will not panic.
func (e *Enforcer) Stop() {
	e.stopOnce.Do(func() { close(e.done) })
}

// sweepLoop removes expired in-flight reservations every second.
// It exits cleanly when Stop() is called.
func (e *Enforcer) sweepLoop() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			e.sweepExpired()
		case <-e.done:
			return
		}
	}
}

// sweepExpired removes all entries whose expiry has passed.
func (e *Enforcer) sweepExpired() {
	now := e.nowFn()
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range e.reservations {
		if now.After(v.expiry) {
			delete(e.reservations, k)
		}
	}
}

// extractGPUs returns the GPU count declared in the resource limits for one
// AgentDeployment replica, using the configured GPU resource name.
// Returns 0 if no GPU limit is set.
func (e *Enforcer) extractGPUs(resources corev1.ResourceRequirements) int64 {
	if resources.Limits == nil {
		return 0
	}
	qty, ok := resources.Limits[corev1.ResourceName(e.gpuResourceName)]
	if !ok {
		return 0
	}
	return qty.Value()
}

// gpusForAD returns the total GPU units for one AgentDeployment:
// gpuPerReplica × spec.replicas.max. The multiplication is performed in
// int64 to prevent overflow, then clamped to the int32 range.
func (e *Enforcer) gpusForAD(ad agentraxv1alpha1.AgentDeploymentSpec) int32 {
	perReplica := e.extractGPUs(ad.Resources)
	total := perReplica * int64(ad.Replicas.Max)
	const maxInt32 = int64(math.MaxInt32)
	if total > maxInt32 {
		return math.MaxInt32
	}
	return int32(total)
}

// ComputeUsage aggregates resource usage across a slice of AgentDeployment specs
// belonging to the same tenant. It is called by the TenantQuota reconciler to
// compute the accurate observed state.
func (e *Enforcer) ComputeUsage(ads []agentraxv1alpha1.AgentDeploymentSpec) agentraxv1alpha1.TenantQuotaStatus {
	var status agentraxv1alpha1.TenantQuotaStatus
	for _, ad := range ads {
		status.UsedAgents++
		status.UsedTotalReplicas += ad.Replicas.Max
		status.UsedGPUs += e.gpusForAD(ad)
	}
	return status
}

// CanAdmit checks whether admitting a new or updated AgentDeployment with the
// given spec would stay within the quota ceilings. It accounts for both the
// already-committed usage (from status) and any in-flight reservations held
// by concurrent admission requests.
//
// admissionKey must be unique per AgentDeployment — use "namespace/adName".
// It identifies the reservation slot for this specific AD so that a retried
// webhook call replaces (not duplicates) any prior reservation, and so that
// sibling ADs in the same TenantQuota each hold independent slots.
// oldSpec may be nil for CREATE requests; for UPDATE requests it is the
// previous spec so we can compute the delta rather than a full new addition.
//
// Returns (true, "") if admission is allowed, or (false, reason) if not.
func (e *Enforcer) CanAdmit(
	admissionKey string,
	quota agentraxv1alpha1.TenantQuotaSpec,
	committedUsage agentraxv1alpha1.TenantQuotaStatus,
	requested agentraxv1alpha1.AgentDeploymentSpec,
	oldSpec *agentraxv1alpha1.AgentDeploymentSpec,
) (bool, string) {
	// Compute the delta this request adds on top of committed usage.
	// For CREATE: delta = full requested resources.
	// For UPDATE: delta = requested - old (can be negative if scaling down).
	var deltaAgents, deltaGPUs, deltaReplicas int32
	if oldSpec == nil {
		// CREATE
		deltaAgents = 1
		deltaGPUs = e.gpusForAD(requested)
		deltaReplicas = requested.Replicas.Max
	} else {
		// UPDATE — agent count doesn't change, only resources may shift.
		deltaAgents = 0
		deltaGPUs = e.gpusForAD(requested) - e.gpusForAD(*oldSpec)
		deltaReplicas = requested.Replicas.Max - oldSpec.Replicas.Max
	}

	// Add in-flight reservations (excluding this AD's own slot, which will
	// be replaced when we Reserve below).
	inFlight := e.sumInflight(admissionKey)

	projectedAgents := committedUsage.UsedAgents + inFlight.agents + deltaAgents
	projectedGPUs := committedUsage.UsedGPUs + inFlight.gpus + deltaGPUs
	projectedReplicas := committedUsage.UsedTotalReplicas + inFlight.replicas + deltaReplicas

	// For UPDATE requests, only reject when the delta increases a dimension that
	// is already at or over quota. If quota was lowered below current usage, the
	// existing ADs are already OverQuota (indicated by the TQ condition) — we
	// must not block updates that don't make things worse, otherwise finalizer
	// removal and spec corrections are deadlocked.
	isUpdate := oldSpec != nil

	if projectedAgents > quota.MaxAgents && (!isUpdate || deltaAgents > 0) {
		return false, fmt.Sprintf(
			"would exceed maxAgents (%d): current=%d in-flight=%d delta=%d",
			quota.MaxAgents, committedUsage.UsedAgents, inFlight.agents, deltaAgents,
		)
	}
	if quota.MaxGPUs > 0 && projectedGPUs > quota.MaxGPUs && (!isUpdate || deltaGPUs > 0) {
		return false, fmt.Sprintf(
			"would exceed maxGPUs (%d): current=%d in-flight=%d delta=%d",
			quota.MaxGPUs, committedUsage.UsedGPUs, inFlight.gpus, deltaGPUs,
		)
	}
	if projectedReplicas > quota.MaxTotalReplicas && (!isUpdate || deltaReplicas > 0) {
		return false, fmt.Sprintf(
			"would exceed maxTotalReplicas (%d): current=%d in-flight=%d delta=%d",
			quota.MaxTotalReplicas, committedUsage.UsedTotalReplicas, inFlight.replicas, deltaReplicas,
		)
	}
	// MaxReplicasPerAgent is only enforced when the request would increase the
	// per-agent replica ceiling. If the quota ceiling was lowered below an
	// existing AD's replicas.max, updates that don't raise replicas.max further
	// must still be allowed — blocking them would deadlock spec corrections.
	if requested.Replicas.Max > quota.MaxReplicasPerAgent && (!isUpdate || requested.Replicas.Max > oldSpec.Replicas.Max) {
		return false, fmt.Sprintf(
			"spec.replicas.max (%d) exceeds maxReplicasPerAgent (%d)",
			requested.Replicas.Max, quota.MaxReplicasPerAgent,
		)
	}

	return true, ""
}

// sumInflight returns the total in-flight resource counts excluding the entry
// for excludeKey (so this AD's existing slot is not double-counted when the
// same AD retries admission).
func (e *Enforcer) sumInflight(excludeKey string) reservationEntry {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.nowFn()
	var total reservationEntry
	for k, v := range e.reservations {
		if k == excludeKey {
			continue
		}
		if now.After(v.expiry) {
			continue
		}
		total.agents += v.agents
		total.gpus += v.gpus
		total.replicas += v.replicas
	}
	return total
}

// Reserve creates or replaces an in-flight reservation for admissionKey (a
// per-request unique string, e.g. UID of the AdmissionRequest) lasting ttl.
// The reservation is automatically swept when it expires. Call Release when
// the webhook handler returns (succeeded or failed), or let it expire on its
// own if the process crashes.
func (e *Enforcer) Reserve(admissionKey string, spec agentraxv1alpha1.AgentDeploymentSpec, oldSpec *agentraxv1alpha1.AgentDeploymentSpec, ttl time.Duration) {
	var deltaAgents, deltaGPUs, deltaReplicas int32
	if oldSpec == nil {
		deltaAgents = 1
		deltaGPUs = e.gpusForAD(spec)
		deltaReplicas = spec.Replicas.Max
	} else {
		deltaGPUs = e.gpusForAD(spec) - e.gpusForAD(*oldSpec)
		deltaReplicas = spec.Replicas.Max - oldSpec.Replicas.Max
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.reservations[admissionKey] = &reservationEntry{
		agents:   deltaAgents,
		gpus:     deltaGPUs,
		replicas: deltaReplicas,
		expiry:   e.nowFn().Add(ttl),
	}
}

// Release removes the in-flight reservation for admissionKey.
// It is safe to call even if the key does not exist.
func (e *Enforcer) Release(admissionKey string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.reservations, admissionKey)
}

// IsOverQuota returns true and a condition message when the committed usage
// exceeds any of the quota ceilings. Used by the TenantQuota reconciler to
// set the OverQuota condition without forcing deletions.
func (e *Enforcer) IsOverQuota(
	quota agentraxv1alpha1.TenantQuotaSpec,
	usage agentraxv1alpha1.TenantQuotaStatus,
) (bool, string) {
	if usage.UsedAgents > quota.MaxAgents {
		return true, fmt.Sprintf("usedAgents (%d) exceeds maxAgents (%d)", usage.UsedAgents, quota.MaxAgents)
	}
	if quota.MaxGPUs > 0 && usage.UsedGPUs > quota.MaxGPUs {
		return true, fmt.Sprintf("usedGPUs (%d) exceeds maxGPUs (%d)", usage.UsedGPUs, quota.MaxGPUs)
	}
	if usage.UsedTotalReplicas > quota.MaxTotalReplicas {
		return true, fmt.Sprintf("usedTotalReplicas (%d) exceeds maxTotalReplicas (%d)", usage.UsedTotalReplicas, quota.MaxTotalReplicas)
	}
	return false, ""
}
