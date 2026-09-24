// Command hive-shadow-diff compares the Rotation controller's Shadow plan with
// what hive-rotate.sh printed for the same spoke.
//
//	kubectl -n hive logs job/<latest hive-rotate job> > bash.log
//	kubectl get hivespoke school -o json > spoke.json
//	hive-shadow-diff --bash bash.log --spoke spoke.json
//
// Both sides are the plan text in hive-rotate.sh's own format (the operator
// stores it in .status.rotationPlanText). The comparison is per agent and
// ignores what only one side can print: the contributors section, footers,
// indented follow-up lines ("    paused: …"), and the "usage: reusing …"
// banner. Exit status: 0 identical, 1 differences, 2 usage error.
//
// A difference is not automatically a bug. Before judging one, check
// .status.rotationInputs and DESIGN.md "Shadow-diff method": the two sides
// must have seen the same tick (both read /api/status within minutes) and the
// same readings (Probe) and ladder, and the operator cannot see bash's
// journals (stranded, pace-demoted, peak-paused, canary cooldown).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

type spoke struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		RotationPlanText []string   `json:"rotationPlanText"`
		RotationInputs   string     `json:"rotationInputs"`
		ObservedAt       *time.Time `json:"observedAt"`
	} `json:"status"`
}

// planLines keeps the per-agent decision lines of a hive-rotate.sh plan/apply
// log, keyed by agent, in order.
func planLines(r io.Reader) map[string][]string {
	out := map[string][]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), " \t\r")
		switch {
		case l == "contributors:":
			return out
		case l == "", strings.HasPrefix(l, " "), strings.HasPrefix(l, "\t"),
			strings.HasPrefix(l, "usage: "), strings.HasPrefix(l, "WARN"), strings.HasPrefix(l, "PEAK"),
			strings.HasPrefix(l, "fleet already on the best available rung"),
			strings.Contains(l, " change(s) — run "):
			continue
		}
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		// "resumed on X (was stranded on Y)" is an apply-only follow-up.
		if f[1] == "resumed" {
			continue
		}
		out[f[0]] = append(out[f[0]], l)
	}
	return out
}

func main() {
	bashPath := flag.String("bash", "", "hive-rotate.sh job log (plan or apply output).")
	spokePath := flag.String("spoke", "", "`kubectl get hivespoke <name> -o json` output.")
	opPath := flag.String("operator", "", "Alternatively: a plain file of operator plan lines.")
	flag.Parse()
	if *bashPath == "" || (*spokePath == "" && *opPath == "") {
		flag.Usage()
		os.Exit(2)
	}
	bf, err := os.Open(*bashPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer bf.Close()
	bash := planLines(bf)

	var op map[string][]string
	if *spokePath != "" {
		b, err := os.ReadFile(*spokePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		var sp spoke
		if err := json.Unmarshal(b, &sp); err != nil {
			fmt.Fprintln(os.Stderr, "parse spoke:", err)
			os.Exit(2)
		}
		if len(sp.Status.RotationPlanText) == 0 {
			fmt.Fprintln(os.Stderr, "spoke has no .status.rotationPlanText — is rotationMode Shadow and the operator new enough?")
			os.Exit(2)
		}
		obs := "?"
		if sp.Status.ObservedAt != nil {
			obs = sp.Status.ObservedAt.UTC().Format(time.RFC3339)
		}
		fmt.Printf("spoke %s  observed %s  inputs: %s\n\n", sp.Metadata.Name, obs, sp.Status.RotationInputs)
		op = planLines(strings.NewReader(strings.Join(sp.Status.RotationPlanText, "\n")))
	} else {
		of, err := os.Open(*opPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		defer of.Close()
		op = planLines(of)
	}

	agents := map[string]bool{}
	for a := range bash {
		agents[a] = true
	}
	for a := range op {
		agents[a] = true
	}
	names := make([]string, 0, len(agents))
	for a := range agents {
		names = append(names, a)
	}
	sort.Strings(names)
	same, diff := 0, 0
	for _, a := range names {
		b, o := strings.Join(bash[a], "\n"), strings.Join(op[a], "\n")
		if b == o {
			same++
			continue
		}
		diff++
		fmt.Printf("≠ %s\n", a)
		for _, l := range bash[a] {
			fmt.Printf("    bash:     %s\n", l)
		}
		if len(bash[a]) == 0 {
			fmt.Printf("    bash:     (no line)\n")
		}
		for _, l := range op[a] {
			fmt.Printf("    operator: %s\n", l)
		}
		if len(op[a]) == 0 {
			fmt.Printf("    operator: (no line)\n")
		}
	}
	fmt.Printf("\n%d agent(s) identical, %d different\n", same, diff)
	if diff > 0 {
		os.Exit(1)
	}
}
