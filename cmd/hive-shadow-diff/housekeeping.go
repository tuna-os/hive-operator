package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
	"github.com/tuna-os/hive-operator/internal/housekeeping"
)

// runHousekeepingLive is `--live --housekeeping`, READ-ONLY:
//
//  1. renders every job of the HiveHousekeeping in --housekeeping-sample and
//     diffs it against the live CronJob (what Shadow reports, without the
//     operator deployed);
//  2. diffs SharedAuth/<name>.status.report against the newest finished
//     hive-shared-auth job log (once the operator writing status.report runs).
//
// Exit 0 = renders match and (if compared) reports match.
func runHousekeepingLive(samplePath, sharedAuth string) int {
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	b, err := os.ReadFile(samplePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var hk hivev1.HiveHousekeeping
	if err := yaml.UnmarshalStrict(b, &hk); err != nil {
		fmt.Fprintln(os.Stderr, "parse:", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ns := housekeeping.Namespace(&hk)

	rc := 0
	fmt.Printf("housekeeping render vs live (ns %s) at %s:\n", ns, time.Now().UTC().Format(time.RFC3339))
	for _, j := range hk.Spec.Jobs {
		live, err := cs.BatchV1().CronJobs(ns).Get(ctx, j.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			fmt.Printf("  %-18s MISSING (Enforce would create it)\n", j.Name)
			rc = 1
			continue
		case err != nil:
			fmt.Printf("  %-18s ERROR %v\n", j.Name, err)
			rc = 1
			continue
		}
		if d := housekeeping.Diff(housekeeping.Render(&hk, j), live); len(d) > 0 {
			fmt.Printf("  %-18s DRIFT %s\n", j.Name, strings.Join(d, ", "))
			rc = 1
		} else {
			fmt.Printf("  %-18s identical (suspend=%v)\n", j.Name, live.Spec.Suspend != nil && *live.Spec.Suspend)
		}
	}
	if sharedAuth == "" {
		return rc
	}

	sch := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sch)
	_ = hivev1.AddToScheme(sch)
	cl, err := client.New(cfg, client.Options{Scheme: sch})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var sa hivev1.SharedAuth
	if err := cl.Get(ctx, client.ObjectKey{Name: sharedAuth}, &sa); err != nil {
		fmt.Printf("SharedAuth/%s: %v\n", sharedAuth, err)
		return 1
	}
	name, log, err := newestJobLog(ctx, cs, ns, "hive-shared-auth")
	if err != nil {
		fmt.Printf("hive-shared-auth log: %v\n", err)
		return 1
	}
	fmt.Printf("\nSharedAuth/%s (effective %s, observed %v) vs job %s:\n", sharedAuth, sa.Status.EffectiveMode, sa.Status.ObservedAt, name)
	if len(sa.Status.Report) == 0 {
		fmt.Println("  status.report is empty — the deployed operator predates it; deploy this build first")
		return 1
	}
	if n := compareReport(sa.Status.Report, strings.Split(strings.TrimRight(log, "\n"), "\n")); n > 0 {
		rc = 1
	}
	return rc
}

// compareReport diffs line sets, ignoring the repair lines only one side can
// print (the bash runs `reconcile`, a Shadow operator `check`).
func compareReport(op, bash []string) int {
	norm := func(ls []string) map[string]bool {
		m := map[string]bool{}
		for _, l := range ls {
			if strings.HasPrefix(l, "theme ") || strings.HasPrefix(l, "agy ") {
				continue
			}
			m[l] = true
		}
		return m
	}
	o, b := norm(op), norm(bash)
	n := 0
	for l := range b {
		if !o[l] {
			fmt.Printf("  bash only:     %s\n", l)
			n++
		}
	}
	for l := range o {
		if !b[l] {
			fmt.Printf("  operator only: %s\n", l)
			n++
		}
	}
	if n == 0 {
		fmt.Println("  identical (theme/agy repair lines excluded: reconcile vs check)")
	}
	return n
}

// newestJobLog returns the log of the newest FINISHED job of a CronJob,
// failed ones included (hive-shared-auth exits 1 on any problem).
func newestJobLog(ctx context.Context, cs kubernetes.Interface, ns, cronjob string) (string, string, error) {
	jobs, err := cs.BatchV1().Jobs(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", err
	}
	var best *batchv1.Job
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if len(j.OwnerReferences) == 0 || j.OwnerReferences[0].Name != cronjob || (j.Status.Succeeded == 0 && j.Status.Failed == 0) {
			continue
		}
		if best == nil || j.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = j
		}
	}
	if best == nil {
		return "", "", fmt.Errorf("no finished job of %s", cronjob)
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + best.Name})
	if err != nil || len(pods.Items) == 0 {
		return best.Name, "", fmt.Errorf("no pod for job %s (%v)", best.Name, err)
	}
	rd, err := cs.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return best.Name, "", err
	}
	defer rd.Close()
	var buf bytes.Buffer
	_, err = io.Copy(&buf, rd)
	return best.Name, buf.String(), err
}
