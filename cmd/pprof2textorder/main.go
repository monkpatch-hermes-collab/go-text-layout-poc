package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/google/pprof/profile"
)

type entry struct {
	Name   string
	Weight int64
}

type stats struct {
	Samples          int
	SamplesZero      int
	SamplesNoName    int
	FunctionsSeen    int
	DroppedMinWeight int
	DroppedRuntime   int
	DroppedUnmatched int
	Emitted          int
}

func main() {
	profilePath := flag.String("profile", "", "CPU pprof profile to read")
	binaryPath := flag.String("binary", "", "optional binary used to filter names to real text symbols via `go tool nm`")
	mode := flag.String("mode", "flat", "hotness mode: flat or cum")
	minWeight := flag.Int64("min-weight", 1, "minimum accumulated sample weight to emit")
	keepRuntime := flag.Bool("keep-runtime", false, "include runtime/internal symbols")
	showStats := flag.Bool("stats", false, "print profile/conversion statistics to stderr")
	unmatchedPath := flag.String("unmatched", "", "optional path to write profile function names not found in binary symbols")
	flag.Parse()

	if *profilePath == "" {
		log.Fatal("-profile is required")
	}
	if *mode != "flat" && *mode != "cum" {
		log.Fatalf("unsupported -mode %q; want flat or cum", *mode)
	}

	f, err := os.Open(*profilePath)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	p, err := profile.Parse(f)
	if err != nil {
		log.Fatal(err)
	}
	weightIndex := chooseWeightIndex(p)

	symbols := map[string]bool(nil)
	if *binaryPath != "" {
		symbols, err = loadTextSymbols(*binaryPath)
		if err != nil {
			log.Fatal(err)
		}
	}

	var st stats
	weights := map[string]int64{}
	for _, s := range p.Sample {
		st.Samples++
		w := sampleWeight(s.Value, weightIndex)
		if w == 0 {
			st.SamplesZero++
			continue
		}
		if *mode == "flat" {
			name := leafFunction(s)
			if name == "" {
				st.SamplesNoName++
				continue
			}
			weights[name] += w
			continue
		}

		seen := map[string]bool{}
		for _, loc := range s.Location {
			for _, line := range loc.Line {
				if line.Function == nil || line.Function.Name == "" {
					continue
				}
				name := line.Function.Name
				if seen[name] {
					continue
				}
				seen[name] = true
				weights[name] += w
			}
		}
		if len(seen) == 0 {
			st.SamplesNoName++
		}
	}
	st.FunctionsSeen = len(weights)

	var unmatched []entry
	var entries []entry
	for name, weight := range weights {
		if weight < *minWeight {
			st.DroppedMinWeight++
			continue
		}
		if !*keepRuntime && isRuntimeName(name) {
			st.DroppedRuntime++
			continue
		}
		matched := matchSymbol(name, symbols)
		if matched == "" {
			st.DroppedUnmatched++
			if symbols != nil {
				unmatched = append(unmatched, entry{Name: name, Weight: weight})
			}
			continue
		}
		entries = append(entries, entry{Name: matched, Weight: weight})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Weight != entries[j].Weight {
			return entries[i].Weight > entries[j].Weight
		}
		return entries[i].Name < entries[j].Name
	})
	for _, e := range entries {
		fmt.Println(e.Name)
	}
	st.Emitted = len(entries)

	if *unmatchedPath != "" {
		if err := writeUnmatched(*unmatchedPath, unmatched); err != nil {
			log.Fatal(err)
		}
	}

	if *showStats || st.Emitted == 0 {
		printStats(os.Stderr, p, weightIndex, symbols, st, *keepRuntime)
	}
}

func chooseWeightIndex(p *profile.Profile) int {
	// CPU profiles commonly have both "samples/count" and "cpu/nanoseconds".
	// Prefer CPU time when present; otherwise fall back to the first sample value.
	for i, st := range p.SampleType {
		if st == nil {
			continue
		}
		if st.Type == "cpu" || st.Unit == "nanoseconds" {
			return i
		}
	}
	return 0
}

func sampleWeight(v []int64, idx int) int64 {
	if len(v) == 0 {
		return 1
	}
	if idx >= 0 && idx < len(v) {
		return v[idx]
	}
	return v[0]
}

func leafFunction(s *profile.Sample) string {
	if len(s.Location) == 0 {
		return ""
	}
	// pprof stores sample locations leaf-first. If inlining is present, the
	// first Line is the innermost frame for the location.
	for _, loc := range s.Location {
		for _, line := range loc.Line {
			if line.Function != nil && line.Function.Name != "" {
				return line.Function.Name
			}
		}
		break
	}
	return ""
}

func loadTextSymbols(binary string) (map[string]bool, error) {
	cmd := exec.Command("go", "tool", "nm", binary)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("go tool nm failed: %v\n%s", err, string(ee.Stderr))
		}
		return nil, err
	}
	return parseNMSymbols(strings.NewReader(string(out))), nil
}

func parseNMSymbols(r io.Reader) map[string]bool {
	syms := map[string]bool{}
	s := bufio.NewScanner(r)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 3 {
			continue
		}
		typ := fields[1]
		if typ != "T" && typ != "t" {
			continue
		}
		name := fields[2]
		syms[name] = true
	}
	return syms
}

func matchSymbol(name string, symbols map[string]bool) string {
	if symbols == nil {
		return name
	}
	candidates := []string{name, strings.ReplaceAll(name, "·", ".")}
	for _, c := range candidates {
		if symbols[c] {
			return c
		}
	}
	return ""
}

func isRuntimeName(name string) bool {
	return strings.HasPrefix(name, "runtime.") ||
		strings.HasPrefix(name, "internal/runtime/") ||
		strings.HasPrefix(name, "internal/abi.") ||
		strings.HasPrefix(name, "internal/cpu.")
}

func writeUnmatched(path string, entries []entry) error {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Weight != entries[j].Weight {
			return entries[i].Weight > entries[j].Weight
		}
		return entries[i].Name < entries[j].Name
	})
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range entries {
		fmt.Fprintf(f, "%d %s\n", e.Weight, e.Name)
	}
	return nil
}

func printStats(w io.Writer, p *profile.Profile, weightIndex int, symbols map[string]bool, st stats, keepRuntime bool) {
	fmt.Fprintln(w, "pprof2textorder stats:")
	fmt.Fprintf(w, "  sample_types:")
	for i, t := range p.SampleType {
		if t == nil {
			continue
		}
		marker := ""
		if i == weightIndex {
			marker = "*"
		}
		fmt.Fprintf(w, " %s%d:%s/%s", marker, i, t.Type, t.Unit)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  samples: %d\n", st.Samples)
	fmt.Fprintf(w, "  samples_zero_weight: %d\n", st.SamplesZero)
	fmt.Fprintf(w, "  samples_without_function_name: %d\n", st.SamplesNoName)
	fmt.Fprintf(w, "  functions_seen: %d\n", st.FunctionsSeen)
	fmt.Fprintf(w, "  dropped_min_weight: %d\n", st.DroppedMinWeight)
	fmt.Fprintf(w, "  dropped_runtime: %d (keep-runtime=%v)\n", st.DroppedRuntime, keepRuntime)
	fmt.Fprintf(w, "  dropped_unmatched: %d\n", st.DroppedUnmatched)
	if symbols != nil {
		fmt.Fprintf(w, "  binary_text_symbols: %d\n", len(symbols))
	} else {
		fmt.Fprintln(w, "  binary_text_symbols: not used")
	}
	fmt.Fprintf(w, "  emitted: %d\n", st.Emitted)
	if st.Emitted == 0 {
		fmt.Fprintln(w, "  hint: if dropped_runtime is high, retry with -keep-runtime")
		fmt.Fprintln(w, "  hint: if samples is 0, collect a longer/active CPU profile")
		fmt.Fprintln(w, "  hint: if samples_without_function_name is high, pass -binary /path/to/unstripped-binary or inspect with go tool pprof")
		fmt.Fprintln(w, "  hint: if dropped_unmatched is high, pass the exact binary used to collect the profile and check -unmatched")
	}
}
