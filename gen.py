#!/usr/bin/env python3
from pathlib import Path
import sys

PAIRS = int(sys.argv[1]) if len(sys.argv) > 1 else 50000
ROOT = Path(__file__).resolve().parent

COMMON_PREFIX = r'''package main

import (
    "bufio"
    "fmt"
    "os"
    "strconv"
    "strings"
)

var Sink uint64

type fn func(uint64) uint64

type metrics struct {
    SizeKB int
    RssKB int
    PssKB int
    SharedCleanKB int
    PrivateCleanKB int
    ReferencedKB int
}

func parseKB(line string) int {
    fields := strings.Fields(line)
    if len(fields) < 2 { return 0 }
    n, _ := strconv.Atoi(fields[1])
    return n
}

func measure(label string) {
    exe, _ := os.Readlink("/proc/self/exe")
    f, err := os.Open("/proc/self/smaps")
    if err != nil { panic(err) }
    defer f.Close()

    var total metrics
    inTarget := false
    scanner := bufio.NewScanner(f)
    scanner.Buffer(make([]byte, 1024), 1024*1024)
    for scanner.Scan() {
        line := scanner.Text()
        fields := strings.Fields(line)
        if len(fields) >= 5 && strings.Contains(fields[1], "r-xp") {
            path := ""
            if len(fields) >= 6 { path = fields[5] }
            inTarget = path == exe
            continue
        }
        if len(fields) >= 5 && strings.Contains(fields[1], "-") && strings.Contains(fields[1], "p") {
            inTarget = false
            continue
        }
        if !inTarget { continue }
        switch {
        case strings.HasPrefix(line, "Size:"):
            total.SizeKB += parseKB(line)
        case strings.HasPrefix(line, "Rss:"):
            total.RssKB += parseKB(line)
        case strings.HasPrefix(line, "Pss:"):
            total.PssKB += parseKB(line)
        case strings.HasPrefix(line, "Shared_Clean:"):
            total.SharedCleanKB += parseKB(line)
        case strings.HasPrefix(line, "Private_Clean:"):
            total.PrivateCleanKB += parseKB(line)
        case strings.HasPrefix(line, "Referenced:"):
            total.ReferencedKB += parseKB(line)
        }
    }
    fmt.Printf("METRIC stage=%s text_size_kb=%d text_rss_kb=%d text_pss_kb=%d shared_clean_kb=%d private_clean_kb=%d referenced_kb=%d sink=%d\n",
        label, total.SizeKB, total.RssKB, total.PssKB, total.SharedCleanKB, total.PrivateCleanKB, total.ReferencedKB, Sink)
}

func run(label string, fns []fn, rounds int) uint64 {
    var acc uint64 = 0x123456789abcdef0
    for r := 0; r < rounds; r++ {
        for _, f := range fns {
            acc ^= f(acc)
        }
    }
    Sink ^= acc
    fmt.Printf("DONE %s acc=%d sink=%d\n", label, acc, Sink)
    return acc
}

'''

MAIN_SUFFIX = r'''
func main() {
    fmt.Printf("pid=%d funcs_hot=%d funcs_cold=%d\n", os.Getpid(), len(hotFns), len(coldFns))
    measure("initial")
    run("hot", hotFns[:], 1)
    measure("after_hot")
    run("cold", coldFns[:], 1)
    measure("after_cold")
}
'''

def func_text(kind, i):
    # Unique constants keep functions distinct; noinline prevents inlining.
    c1 = (0x9e3779b97f4a7c15 + i * 0x100000001b3 + (1 if kind == 'hot' else 0x5555)) & ((1<<64)-1)
    c2 = (0xbf58476d1ce4e5b9 ^ (i * 0x94d049bb133111eb) ^ (0 if kind == 'hot' else 0xaaaaaaaaaaaaaaaa)) & ((1<<64)-1)
    return f'''
//go:noinline
func {kind}{i:06d}(x uint64) uint64 {{
    x += 0x{c1:016x}
    x ^= x << 13
    x ^= x >> 7
    x *= 0x{c2:016x}
    x ^= x >> 17
    if x == 0x{(c1^c2):016x} {{
        Sink ^= x
    }}
    return x
}}
'''

def array_text(name, kind):
    out = [f"var {name} = [...]fn{{\n"]
    for i in range(PAIRS):
        out.append(f"    {kind}{i:06d},\n")
    out.append("}\n")
    return ''.join(out)

def write_variant(variant):
    d = ROOT / 'cmd' / variant
    d.mkdir(parents=True, exist_ok=True)
    chunks = [COMMON_PREFIX]
    if variant == 'interleaved':
        for i in range(PAIRS):
            chunks.append(func_text('hot', i))
            chunks.append(func_text('cold', i))
    elif variant == 'grouped':
        for i in range(PAIRS):
            chunks.append(func_text('hot', i))
        for i in range(PAIRS):
            chunks.append(func_text('cold', i))
    else:
        raise ValueError(variant)
    chunks.append(array_text('hotFns', 'hot'))
    chunks.append(array_text('coldFns', 'cold'))
    chunks.append(MAIN_SUFFIX)
    (d / 'main.go').write_text(''.join(chunks))
    print(f"wrote {d/'main.go'} with {PAIRS*2} funcs")

for v in ['interleaved', 'grouped']:
    write_variant(v)

(ROOT / 'go.mod').write_text('module example.com/go-text-layout-poc\n\ngo 1.22\n')
