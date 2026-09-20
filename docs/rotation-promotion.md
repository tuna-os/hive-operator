# Promoting rotation from Shadow

The fleet entered its one-week shadow window on 2026-09-20. The earliest
planned promotion is 2026-09-27.

Before promotion, inspect every proposed plan and controller errors:

```sh
kubectl get hivespokes -o yaml
kubectl -n hive-operator-system logs deploy/hive-operator-controller-manager --since=24h
```

Promote only when every proposed backend/model exists in live inventory, no
agent is proposed for an exhausted provider, and all three spokes are reachable.
The operator currently owns rotation only; the shell watchdog remains active.

Promote the spokes and suspend only the legacy rotation jobs in one maintenance
window:

```sh
for spoke in school reef hanthor; do
  kubectl patch hivespoke "$spoke" --type merge -p '{"spec":{"rotationMode":"Enforce"}}'
done
kubectl -n hive patch cronjob hive-rotate -p '{"spec":{"suspend":true}}'
kubectl -n hive-reef patch cronjob hive-rotate-reef -p '{"spec":{"suspend":true}}'
kubectl -n hive-hanthor patch cronjob hive-rotate-hanthor -p '{"spec":{"suspend":true}}'
```

To roll back, set every spoke to `Shadow` and set the three CronJobs' `suspend`
fields to `false`. Do not suspend the watchdog jobs until watchdog ownership is
implemented in the operator.
