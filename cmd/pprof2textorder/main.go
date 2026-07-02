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

func main() {
	profilePath := flag.String("profile", "", "CPU pprof profile to read")
	binaryPath := flag.String("binary", "", "optional binary used to filter names to real text symbols via `go tool nm`")
	mode := flag.String("mode", "flat", "hotness mode: flat or cum")
	minWeight := flag.Int64("min-weight", 1, "minimum accumulated sample weight to emit")
	keepRuntime := flag.Bool("keep-runtime", false, "include runtime/internal symbols")
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

	symbols := map[string]bool(nil)
	if *binaryPath != "" {
		symbols, err = loadTextSymbols(*binaryPath)
		if err != nil {
			log.Fatal(err)
		}
	}

	weights := map[string]int64{}
	for _, s := range p.Sample {
		w := sampleWeight(s.Value)
		if w == 0 {
			continue
		}
		if *mode == "flat" {
			name := leafFunction(s)
			if name != "" {
				weights[name] += w
			}
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
	}

	var unmatched []entry
	var entries []entry
	for name, weight := range weights {
		if weight < *minWeight || (!*keepRuntime && isRuntimeName(name)) {
			continue
		}
		matched := matchSymbol(name, symbols)
		if matched == "" {
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

	if *unmatchedPath != "" {
		if err := writeUnmatched(*unmatchedPath, unmatched); err != nil {
			log.Fatal(err)
		}
	}
}

func sampleWeight(v []int64) int64 {
	if len(v) == 0 {
		return 1
	}
	// For CPU profiles this is normally sample count or nanoseconds depending on
	// profile/sample type. For ordering, either is usable as a relative weight.
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
