// Package dashboard serves a read-only fleet view.
//
// It renders live CR status, not a cache, so what it shows is what the
// controllers last observed. Every panel here exists because the corresponding
// failure was invisible during an eight-hour outage: idle time, budget
// suppression, credential presence and share consistency.
package dashboard

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	hivev1 "github.com/tuna-os/hive-operator/api/v1alpha1"
)

// Server renders the fleet view.
type Server struct {
	Client client.Client
}

type agentRow struct {
	Spoke, Name, Backend, Model, Trigger string
	Paused, Pinned, OnDemand             bool
	Idle                                 string
	IdleWarn                             bool
}

type spokeRow struct {
	Name, Namespace, Org, Budget string
	Reachable, Exhausted         bool
	Running, Paused, OnDemand    int
	Observed                     string
}

type authRow struct {
	Store, Namespace string
	Dirs             string
	Cred, AllShared  bool
	Message          string
}

type page struct {
	Spokes  []spokeRow
	Agents  []agentRow
	Auth    []authRow
	Pending []string
	Now     string
}

func humanAge(s int64) (string, bool) {
	if s < 0 {
		return "never", true
	}
	d := time.Duration(s) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", s), false
	case d < time.Hour:
		return fmt.Sprintf("%dm", s/60), false
	default:
		return fmt.Sprintf("%dh%dm", s/3600, (s%3600)/60), d > 4*time.Hour
	}
}

// Handler returns the HTTP handler for the dashboard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		var p page
		p.Now = time.Now().UTC().Format(time.RFC3339)

		var spokes hivev1.HiveSpokeList
		if err := s.Client.List(ctx, &spokes); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, sp := range spokes.Items {
			row := spokeRow{
				Name: sp.Name, Namespace: sp.Spec.Namespace, Org: sp.Spec.Org,
				Reachable: sp.Status.Reachable, Exhausted: sp.Status.BudgetExhausted,
				Budget: sp.Status.BudgetPctUsed,
			}
			if sp.Status.ObservedAt != nil {
				row.Observed = sp.Status.ObservedAt.UTC().Format("15:04:05Z")
			}
			for _, a := range sp.Status.Agents {
				switch {
				case a.OnDemand:
					row.OnDemand++
				case a.Paused:
					row.Paused++
				default:
					row.Running++
				}
				idle, warn := humanAge(a.IdleSeconds)
				p.Agents = append(p.Agents, agentRow{
					Spoke: sp.Name, Name: a.Name, Backend: a.Backend, Model: a.Model,
					Trigger: a.PausedTrigger, Paused: a.Paused, Pinned: a.Pinned,
					OnDemand: a.OnDemand, Idle: idle, IdleWarn: warn,
				})
			}
			p.Spokes = append(p.Spokes, row)
		}

		var auths hivev1.SharedAuthList
		if err := s.Client.List(ctx, &auths); err == nil {
			for _, sa := range auths.Items {
				p.Pending = append(p.Pending, sa.Status.PendingRepairs...)
				for _, ns := range sa.Status.Namespaces {
					all := len(ns.Shared) > 0
					dirs := ""
					keys := make([]string, 0, len(ns.Shared))
					for d := range ns.Shared {
						keys = append(keys, d)
					}
					sort.Strings(keys)
					for _, d := range keys {
						ok := ns.Shared[d]
						if !ok {
							all = false
						}
						mark := "ok"
						if !ok {
							mark = "NO"
						}
						dirs += fmt.Sprintf("%s:%s ", d, mark)
					}
					p.Auth = append(p.Auth, authRow{
						Store: sa.Name, Namespace: ns.Namespace, Dirs: dirs,
						Cred: ns.CredentialPresent, AllShared: all, Message: ns.Message,
					})
				}
			}
		}
		sort.Slice(p.Agents, func(i, j int) bool {
			if p.Agents[i].Spoke != p.Agents[j].Spoke {
				return p.Agents[i].Spoke < p.Agents[j].Spoke
			}
			return p.Agents[i].Name < p.Agents[j].Name
		})

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, p); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	return mux
}

var tmpl = template.Must(template.New("p").Parse(`<!doctype html>
<meta charset="utf-8"><title>hive fleet</title>
<style>
 :root{color-scheme:light dark}
 body{font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;margin:2rem;max-width:1100px}
 h1{font-size:1.2rem} h2{font-size:1rem;margin-top:2rem}
 table{border-collapse:collapse;width:100%;margin:.5rem 0}
 th,td{text-align:left;padding:.25rem .6rem;border-bottom:1px solid #8883}
 th{font-weight:600;opacity:.7}
 .bad{color:#c0392b;font-weight:600}.warn{color:#b9770e;font-weight:600}.ok{opacity:.65}
 .pill{border:1px solid #8886;border-radius:.6rem;padding:0 .4rem;font-size:.85em}
 ul{padding-left:1.2rem}
</style>
<h1>hive fleet <span class="ok">— observed {{.Now}}</span></h1>

<h2>spokes</h2>
<table><tr><th>spoke<th>ns<th>org<th>running<th>paused<th>on-demand<th>budget<th>state<th>seen</tr>
{{range .Spokes}}<tr>
<td>{{.Name}}<td>{{.Namespace}}<td>{{.Org}}<td>{{.Running}}<td>{{.Paused}}<td>{{.OnDemand}}<td>{{.Budget}}
<td>{{if not .Reachable}}<span class="bad">unreachable</span>{{else if .Exhausted}}<span class="bad">budget exhausted</span>{{else}}<span class="ok">ok</span>{{end}}
<td>{{.Observed}}</tr>{{end}}</table>

<h2>agents</h2>
<table><tr><th>spoke<th>agent<th>backend<th>model<th>idle<th>state</tr>
{{range .Agents}}<tr>
<td>{{.Spoke}}<td>{{.Name}}{{if .Pinned}} <span class="pill">pinned</span>{{end}}
<td>{{.Backend}}<td>{{.Model}}
<td>{{if .IdleWarn}}<span class="warn">{{.Idle}}</span>{{else}}{{.Idle}}{{end}}
<td>{{if .OnDemand}}<span class="ok">on-demand</span>{{else if .Paused}}<span class="bad">paused ({{.Trigger}})</span>{{else}}<span class="ok">running</span>{{end}}</tr>{{end}}</table>

<h2>shared credentials</h2>
<table><tr><th>store<th>namespace<th>dirs (write-through)<th>token</tr>
{{range .Auth}}<tr>
<td>{{.Store}}<td>{{.Namespace}}
<td>{{if .AllShared}}<span class="ok">{{.Dirs}}</span>{{else}}<span class="bad">{{.Dirs}}</span>{{end}}{{if .Message}} <span class="warn">{{.Message}}</span>{{end}}
<td>{{if .Cred}}<span class="ok">present</span>{{else}}<span class="bad">EMPTY</span>{{end}}</tr>{{end}}</table>

{{if .Pending}}<h2>pending / needs a human</h2><ul>{{range .Pending}}<li class="warn">{{.}}</li>{{end}}</ul>{{end}}
`))
