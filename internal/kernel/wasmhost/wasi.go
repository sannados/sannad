package wasmhost

import (
	"context"
	"encoding/binary"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// A guest compiled from a real language needs a handful of WASI calls before its
// runtime will start: Go's wasip1 target imports sixteen. The obvious way to
// satisfy them is to instantiate wazero's WASI implementation, and that is the
// wrong answer for this tier — it would hand untrusted code a filesystem, a
// clock, an environment, and the host's entropy.
//
// So this is a shim rather than an implementation. Every function exists so a
// guest can start; almost every one refuses to do anything. The distinction that
// matters is between *inert* and *denied*:
//
//   - Inert: args and environment are empty, sched_yield succeeds, proc_exit
//     traps. A guest asking these questions gets a truthful "nothing here".
//   - Denied: every filesystem call returns ENOTCAPABLE. A guest cannot open,
//     read, or stat anything, because there is nothing to open — and the error
//     says so rather than pretending a file was missing.
//
// Two are deliberately partial and worth being explicit about:
//
//   - clock_time_get returns a fixed value. A real clock is a side channel and a
//     source of nondeterminism, and a validation or pricing rule — the cases ADR
//     0001 names for this tier — has no business reading wall time. A guest that
//     needs a timestamp should receive it in its request, where the host controls
//     it.
//   - random_get fills zeros. A guest needing entropy is doing something this
//     tier does not support; giving it predictable bytes is safer than giving it
//     the host's pool, and a guest generating keys with it will produce visibly
//     broken output rather than subtly weak output.
//
// fd_write to stdout and stderr is the one real capability: without it a guest
// panic is silent, and a plugin author debugging a sandbox with no output has
// nothing to work with. It is bounded by the guest's own memory.

// WASI error numbers, as the ABI defines them.
const (
	wasiESuccess    uint32 = 0
	wasiEBadF       uint32 = 8
	wasiENotCapable uint32 = 76
)

// wasiFileDescriptors a guest may write to.
const (
	fdStdout = 1
	fdStderr = 2
)

// instantiateWASI wires the shim into a runtime.
//
// Named for what it is: the minimum that lets a compiled guest boot, not an
// implementation of WASI. Anything a guest could use to reach outside the
// sandbox is refused rather than omitted, because a missing import fails at
// instantiation with a message about linking rather than about permission.
func instantiateWASI(ctx context.Context, runtime wazero.Runtime, stdout, stderr func([]byte)) error {
	builder := runtime.NewHostModuleBuilder("wasi_snapshot_preview1")

	// --- Inert: truthful answers to questions with nothing behind them ---

	// args_get / args_sizes_get: no arguments. Writing zero counts is what tells
	// a guest runtime there is nothing to read.
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, api.Module, uint32, uint32) uint32 { return wasiESuccess }).
		Export("args_get")
	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, countPtr, bufPtr uint32) uint32 {
			writeU32(mod, countPtr, 0)
			writeU32(mod, bufPtr, 0)
			return wasiESuccess
		}).Export("args_sizes_get")

	// environ_get / environ_sizes_get: no environment. A guest must not learn
	// anything about the host it runs on, and environment variables are where
	// credentials live.
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, api.Module, uint32, uint32) uint32 { return wasiESuccess }).
		Export("environ_get")
	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, countPtr, bufPtr uint32) uint32 {
			writeU32(mod, countPtr, 0)
			writeU32(mod, bufPtr, 0)
			return wasiESuccess
		}).Export("environ_sizes_get")

	// sched_yield: harmless, and a Go guest calls it during scheduling.
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context) uint32 { return wasiESuccess }).
		Export("sched_yield")

	// proc_exit: a guest ending itself. Closing the module turns it into a trap
	// the host reports, rather than letting a guest terminate the process — which
	// is what os.Exit does at tier 1 and is one of the reasons this tier exists.
	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, code uint32) {
			_ = mod.CloseWithExitCode(ctx, code)
		}).Export("proc_exit")

	// poll_oneoff: a guest waiting on events. There are none, and returning
	// success with nothing ready keeps a runtime's scheduler moving instead of
	// deadlocking it.
	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, in, out, nsubs, resultPtr uint32) uint32 {
			writeU32(mod, resultPtr, 0)
			return wasiESuccess
		}).Export("poll_oneoff")

	// --- Deliberately degraded ---

	// clock_time_get: a monotonic counter, not the host's clock.
	//
	// A frozen clock is not an option — the Go runtime refuses to start with one
	// ("fatal error: nanotime returning zero"), and any guest language with a
	// scheduler is likely to be similar. So the shim advances a counter instead
	// of reading the wall clock.
	//
	// A guest can therefore measure elapsed time inside one invocation, which it
	// needs to run at all. What it cannot learn is the actual date, the host's
	// timezone, or anything correlatable with events outside the sandbox — the
	// counter starts from the same base every invocation, so it carries no
	// information across calls or between tenants.
	//
	// A guest needing a real timestamp should receive it in its request, where
	// the host decides what to reveal.
	var monotonic atomic.Uint64
	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, id uint32, precision uint64, resultPtr uint32) uint32 {
			// A microsecond per call: enough for a scheduler to see progress,
			// and unrelated to how much time actually passed.
			now := monotonic.Add(1000)
			if mem := mod.Memory(); mem != nil {
				mem.WriteUint64Le(resultPtr, now)
			}
			return wasiESuccess
		}).Export("clock_time_get")

	// random_get: zeros. A guest that needs entropy is outside what this tier
	// supports, and predictable bytes fail visibly rather than subtly.
	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, bufPtr, bufLen uint32) uint32 {
			if mem := mod.Memory(); mem != nil {
				mem.Write(bufPtr, make([]byte, bufLen))
			}
			return wasiESuccess
		}).Export("random_get")

	// --- Denied: anything that would reach a filesystem ---

	// The prestat calls enumerate preopened directories, and a guest runtime walks
	// them at startup until one fails. There are none, so the walk must end
	// immediately.
	//
	// EBADF, not ENOTCAPABLE: EBADF is what a real WASI host returns when the
	// enumeration runs off the end, and it is what a guest runtime is written to
	// expect. ENOTCAPABLE reads as "this descriptor exists but you may not use
	// it", and Go's runtime treats that as fatal — it panicked with
	// "fd_prestat: Capabilities insufficient" before this was corrected. Saying
	// "there is no such descriptor" is both true and the answer a guest can act
	// on.
	//
	// The two calls take different argument counts; a mismatch is caught at
	// instantiation rather than when a guest calls one.
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, api.Module, uint32, uint32) uint32 { return wasiEBadF }).
		Export("fd_prestat_get")
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, api.Module, uint32, uint32, uint32) uint32 { return wasiEBadF }).
		Export("fd_prestat_dir_name")
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, api.Module, uint32) uint32 { return wasiEBadF }).
		Export("fd_close")
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, api.Module, uint32, uint32) uint32 { return wasiENotCapable }).
		Export("fd_fdstat_get")
	builder.NewFunctionBuilder().
		WithFunc(func(context.Context, api.Module, uint32, uint32) uint32 { return wasiENotCapable }).
		Export("fd_fdstat_set_flags")

	// fd_write: stdout and stderr only, so a guest panic is visible. Anything
	// else is a file descriptor that does not exist.
	builder.NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, fd, iovsPtr, iovsLen, resultPtr uint32) uint32 {
			if fd != fdStdout && fd != fdStderr {
				return wasiEBadF
			}
			mem := mod.Memory()
			if mem == nil {
				return wasiEBadF
			}

			var written uint32
			for i := uint32(0); i < iovsLen; i++ {
				base := iovsPtr + i*8
				ptr, ok := mem.ReadUint32Le(base)
				if !ok {
					return wasiEBadF
				}
				length, ok := mem.ReadUint32Le(base + 4)
				if !ok {
					return wasiEBadF
				}
				// Bounds-checked by wazero against the guest's own memory, so a
				// bogus iovec cannot make the host read outside the sandbox.
				chunk, ok := mem.Read(ptr, length)
				if !ok {
					return wasiEBadF
				}
				if sink := sinkFor(fd, stdout, stderr); sink != nil {
					buf := make([]byte, len(chunk))
					copy(buf, chunk)
					sink(buf)
				}
				written += length
			}
			writeU32(mod, resultPtr, written)
			return wasiESuccess
		}).Export("fd_write")

	_, err := builder.Instantiate(ctx)
	return err
}

func sinkFor(fd uint32, stdout, stderr func([]byte)) func([]byte) {
	if fd == fdStdout {
		return stdout
	}
	return stderr
}

func writeU32(mod api.Module, ptr, value uint32) {
	mem := mod.Memory()
	if mem == nil {
		return
	}
	var buf [4]byte
	binary.LittleEndian.PutUint32(buf[:], value)
	mem.Write(ptr, buf[:])
}
