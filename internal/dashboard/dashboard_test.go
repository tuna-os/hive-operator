package dashboard

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

func TestPoolRows(t *testing.T) {
	now := time.Date(2026, 9, 24, 17, 12, 0, 0, time.UTC)
	eta := metav1.NewTime(now.Add(90 * time.Minute))
	pools := []hivev1.UsagePool{{
		ObjectMeta: metav1.ObjectMeta{Name: "anthropic"},
		Spec:       hivev1.UsagePoolSpec{Provider: "anthropic"},
		Status: hivev1.UsagePoolStatus{
			Windows: []hivev1.UsageWindowStatus{
				{Name: "5h", UsedPercent: "91.0000", ReadingPercent: "91.0000", Consumed: "60.1000", Limit: "66.5882", LimitSource: "learned", Remaining: "6.4882", BurnPerHour: "4.3000", ExhaustionETA: &eta},
				{Name: "weekly", UsedPercent: "-1.0000", ReadingPercent: "-1", Consumed: "36.85", LimitSource: "none"},
			},
			Agents: []hivev1.AgentUsage{{Agent: "sec-check", Window: "5h", Consumed: "30.0000"}},
		},
	}}
	rows := poolRows(pools, now)
	if len(rows) != 2 || !rows[0].Hot || rows[0].ETA != "1h30m0s" || rows[0].Agents != "sec-check 30.00" {
		t.Fatalf("%+v", rows[0])
	}
	if !rows[1].Unknown || rows[1].Limit != "—" || rows[1].Used != "—" {
		t.Fatalf("unmeasured window must render as unmeasured, not 0 or 100: %+v", rows[1])
	}
}
