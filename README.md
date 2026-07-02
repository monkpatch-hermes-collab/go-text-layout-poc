# Go text layout PoC

This repository contains a small proof of concept for ordering Go text symbols by profile/order-file data at link time.

The experiment checks whether packing frequently executed functions together can reduce resident executable pages for large Go binaries whose hot and cold functions are otherwise interleaved.

## Contents

- `gen.py` — generates synthetic `interleaved` and `grouped` Go programs.
- `cmd/pprof2textorder` — converts a CPU pprof profile into a linker text order file.
- `go-link-textorder-poc.patch` — experimental patch for `cmd/link` adding `-textorder=file`.

Generated synthetic sources and binaries are ignored by git.

## Synthetic benchmark

Generate 50k hot + 50k cold functions:

```bash
python3 gen.py 50000
```

Build baseline binaries with the stock Go toolchain:

```bash
mkdir -p bin
go build -trimpath -ldflags='-s -w' -o bin/interleaved ./cmd/interleaved
go build -trimpath -ldflags='-s -w' -o bin/grouped ./cmd/grouped
```

Run and compare executable mapping RSS reported from `/proc/self/smaps`:

```bash
./bin/interleaved
./bin/grouped
```

Observed on linux/amd64 with 100k functions:

```text
interleaved initial:     636 KiB
interleaved after_hot:  9916 KiB
interleaved after_cold: 9916 KiB

grouped initial:         636 KiB
grouped after_hot:      5308 KiB
grouped after_cold:     9916 KiB
```

With Go 1.26.0 and the linker patch:

```text
interleaved initial:                 752 KiB
interleaved after_hot:             10032 KiB
interleaved after_cold:            10032 KiB

grouped initial:                     752 KiB
grouped after_hot:                  5424 KiB
grouped after_cold:                10032 KiB

interleaved + -textorder initial:    752 KiB
interleaved + -textorder after_hot: 5424 KiB
interleaved + -textorder after_cold:10032 KiB
```

The linker-level order file produced the same resident `.text` reduction as source-level grouping.

## Applying the linker patch

Clone Go and apply the patch:

```bash
git clone https://github.com/golang/go.git gosrc
cd gosrc
git switch --detach go1.26.0
git apply /path/to/go-link-textorder-poc.patch
cd src
GOROOT_BOOTSTRAP=/path/to/go1.25 ./make.bash
```

The patched linker accepts:

```bash
-ldflags='-textorder=/path/to/order.txt'
```

The order file is one linker symbol name per line:

```text
main.hot000000
main.hot000001
main.hot000002
```

Known symbols are moved to the front in listed order; unknown symbols keep their relative order after the ordered symbols.

## Reproducing the linker-level synthetic result

```bash
python3 gen.py 50000
python3 - <<'PY' > hot.order
for i in range(50000):
    print(f"main.hot{i:06d}")
PY

/path/to/patched/go build \
  -trimpath \
  -ldflags='-s -w -textorder=/absolute/path/to/hot.order' \
  -o bin/interleaved.textorder \
  ./cmd/interleaved

./bin/interleaved.textorder
```

Verify layout with symbols kept:

```bash
/path/to/patched/go build \
  -trimpath \
  -ldflags='-textorder=/absolute/path/to/hot.order' \
  -o bin/interleaved.textorder.sym \
  ./cmd/interleaved

go tool nm -n bin/interleaved.textorder.sym | grep -E ' main\.(hot|cold)' | head
```

## pprof to text order

Build converter:

```bash
go build ./cmd/pprof2textorder
```

Convert a CPU profile into a text order file:

```bash
./pprof2textorder \
  -profile cpu.pprof \
  -binary ./app.base \
  -mode flat \
  -unmatched unmatched.txt \
  > hot.order
```

Modes:

- `flat` — use the leaf frame of each sample. Best first approximation for executed-code hotness.
- `cum` — add weight to all frames in each sample stack.

Then build with the patched linker:

```bash
/path/to/patched/go build \
  -ldflags="-textorder=$PWD/hot.order" \
  -o app.reordered \
  ./cmd/app
```

Recommended workflow:

1. Build `app.base` without text ordering.
2. Collect representative CPU profile from `app.base`.
3. Convert profile to `hot.order` with `pprof2textorder`.
4. Build `app.reordered` with `-textorder`.
5. Compare `/proc/$pid/smaps`, page faults, iTLB misses, and latency/throughput.

## Status

This is a PoC, not a production linker change. Known missing pieces:

- robust tests inside the Go linker tree;
- more conservative runtime/internal symbol handling;
- strategy experiments: flat sort vs cumulative sort vs buckets/call-graph clustering;
- profile name normalization for wrappers, generics, and inlined frames;
- architecture-specific trampoline and split-section stress tests.
