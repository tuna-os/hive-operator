package controller

import (
	"context"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/rotation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *HiveSpokeReconciler) qualityBudgetBlocks(ctx context.Context, in *rotation.Input) {
	if in.MinimumScore == "" {
		return
	}
	in.PaceBlocked = map[string]bool{}
	var pools hivev1.UsagePoolList
	if err := r.List(ctx, &pools); err != nil {
		for _, rung := range in.Rungs {
			in.PaceBlocked[rung.Provider] = true
		}
		return
	}
	for _, pool := range pools.Items {
		if p := pool.Status.Pace; p != nil {
			if p.Verdict == "hot" || (pool.Spec.Provider == "kiro" && kiroFromStatus(p).Verdict == "over") {
				in.PaceBlocked[pool.Spec.Provider] = true
			}
		}
	}
}

// Each spoke may reference a different ladder. Never apply the reconciling
// spoke's eligibility to another spoke in a fleet-wide pace decision.
func spokeQuality(ctx context.Context, c client.Client, sp *hivev1.HiveSpoke) (string, []rotation.Rung) {
	name := sp.Spec.LadderRef
	if name == "" {
		name = "fleet"
	}
	var ladder hivev1.ModelLadder
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &ladder); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil // no ladder object means no explicit opt-in policy
		}
		return "unavailable", nil // fail closed when policy cannot be read
	}
	return ladder.Spec.MinimumScore, ladderRungs(ladder.Status.Effective)
}
