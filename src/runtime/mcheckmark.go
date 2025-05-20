// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// GC checkmarks
//
// In a concurrent garbage collector, one worries about failing to mark
// a live object due to mutations without write barriers or bugs in the
// collector implementation. As a sanity check, the GC has a 'checkmark'
// mode that retraverses the object graph with the world stopped, to make
// sure that everything that should be marked is marked.

package runtime

import (
	"internal/goarch"
	"internal/runtime/atomic"
	"internal/runtime/sys"
	"unsafe"
)

// A checkmarksMap stores the GC marks in "checkmarks" mode. It is a
// per-arena bitmap with a bit for every word in the arena. The mark
// is stored on the bit corresponding to the first word of the marked
// allocation.
type checkmarksMap struct {
	_ sys.NotInHeap
	b [heapArenaBytes / goarch.PtrSize / 8]uint8
}

// If useCheckmark is true, marking of an object uses the checkmark
// bits instead of the standard mark bits.
var useCheckmark = false

// startCheckmarks prepares for the checkmarks phase.
//
// The world must be stopped.
func startCheckmarks() {
	assertWorldStopped()

	// Clear all checkmarks.
	clearCheckmarks := func(ai arenaIdx) {
		arena := mheap_.arenas[ai.l1()][ai.l2()]
		bitmap := arena.checkmarks

		if bitmap == nil {
			// Allocate bitmap on first use.
			bitmap = (*checkmarksMap)(persistentalloc(unsafe.Sizeof(*bitmap), 0, &memstats.gcMiscSys))
			if bitmap == nil {
				throw("out of memory allocating checkmarks bitmap")
			}
			arena.checkmarks = bitmap
		} else {
			// Otherwise clear the existing bitmap.
			clear(bitmap.b[:])
		}
	}
	for _, ai := range mheap_.heapArenas {
		clearCheckmarks(ai)
	}
	for _, ai := range mheap_.userArenaArenas {
		clearCheckmarks(ai)
	}

	// Enable checkmarking.
	useCheckmark = true
}

// endCheckmarks ends the checkmarks phase.
func endCheckmarks() {
	if gcMarkWorkAvailable(nil) {
		throw("GC work not flushed")
	}
	useCheckmark = false
}

// setCheckmark throws if marking object is a checkmarks violation,
// and otherwise sets obj's checkmark. It returns true if obj was
// already checkmarked.
func setCheckmark(obj, base, off uintptr, mbits markBits) bool {
	if !mbits.isMarked() {
		printlock()
		print("runtime: checkmarks found unexpected unmarked object obj=", hex(obj), "\n")
		print("runtime: found obj at *(", hex(base), "+", hex(off), ")\n")

		// Dump the source (base) object
		gcDumpObject("base", base, off)

		// Dump the object
		gcDumpObject("obj", obj, ^uintptr(0))

		getg().m.traceback = 2
		throw("checkmark found unmarked object")
	}
	bytep, mask := getCheckmark(obj)
	if bytep == nil {
		return false
	}
	if atomic.Load8(bytep)&mask != 0 {
		// Already checkmarked.
		return true
	}
	atomic.Or8(bytep, mask)
	return false
}

func getCheckmark(obj uintptr) (bytep *byte, mask uint8) {
	ai := arenaIndex(obj)
	arena := mheap_.arenas[ai.l1()][ai.l2()]
	if arena == nil {
		// Non-heap pointer.
		return nil, 0
	}
	wordIdx := (obj - alignDown(obj, heapArenaBytes)) / goarch.PtrSize
	arenaWord := wordIdx / 8
	mask = byte(1 << (wordIdx % 8))
	bytep = &arena.checkmarks.b[arenaWord]
	return bytep, mask
}

// runCheckmark runs a full non-parallel, stop-the-world mark using
// checkmark bits, to check that we didn't forget to mark anything
// during the concurrent mark process.
//
// The world must be stopped to call runCheckmark.
func runCheckmark(prepareRootSet func(*gcWork)) {
	assertWorldStopped()

	// Turn off gcwaiting because that will force
	// gcDrain to return early if this goroutine
	// happens to have its preemption flag set.
	// This is fine because the world is stopped.
	// Restore it after we're done just to be safe.
	sched.gcwaiting.Store(false)
	startCheckmarks()
	gcResetMarkState()
	gcw := &getg().m.p.ptr().gcw
	prepareRootSet(gcw)
	gcDrain(gcw, 0)
	wbBufFlush1(getg().m.p.ptr())
	gcw.dispose()
	endCheckmarks()
	sched.gcwaiting.Store(true)
}

// checkFinalizersAndCleanups uses checkmarks to check for potential issues
// with the program's use of cleanups and finalizers.
func checkFinalizersAndCleanups() {
	assertWorldStopped()

	type report struct {
		ptr uintptr
		sp  *special
	}
	var reports [25]report
	var nreports int
	var more bool

	forEachSpecial(func(p uintptr, s *mspan, sp *special) bool {
		// We only care about finalizers and cleanups.
		if sp.kind != _KindSpecialFinalizer && sp.kind != _KindSpecialCleanup {
			return true
		}

		// Run a checkmark GC using this cleanup and/or finalizer as a root.
		runCheckmark(func(gcw *gcWork) {
			switch sp.kind {
			case _KindSpecialFinalizer:
				gcScanFinalizer((*specialfinalizer)(unsafe.Pointer(sp)), s, gcw)
			case _KindSpecialCleanup:
				gcScanCleanup((*specialCleanup)(unsafe.Pointer(sp)), gcw)
			}
		})

		// Now check to see if the object the special is attached to was marked.
		// The roots above do not directly mark p, so if it is marked, then p
		// must be reachable from the finalizer and/or cleanup, preventing
		// reclamation.
		bytep, mask := getCheckmark(p)
		if bytep == nil {
			return true
		}
		if atomic.Load8(bytep)&mask != 0 {
			if nreports >= len(reports) {
				more = true
				return false
			}
			reports[nreports] = report{p, sp}
			nreports++
		}
		return true
	})

	if nreports > 0 {
		lastPtr := uintptr(0)
		for _, r := range reports[:nreports] {
			var ctx *specialCheckFinalizer
			var kind string
			if r.sp.kind == _KindSpecialFinalizer {
				kind = "finalizer"
				ctx = getCleanupContext(r.ptr, 0)
			} else {
				kind = "cleanup"
				ctx = getCleanupContext(r.ptr, ((*specialCleanup)(unsafe.Pointer(r.sp))).id)
			}

			// N.B. reports is sorted 'enough' that cleanups/finalizers on the same pointer will
			// appear consecutively because the specials list is sorted.
			if lastPtr != r.ptr {
				if lastPtr != 0 {
					println()
				}
				print("runtime: value of type ", toRType(ctx.ptrType).string(), " @ ", hex(r.ptr), " is reachable from cleanup or finalizer\n")
				println("value reachable from function or argument at one of:")
			}

			funcInfo := findfunc(ctx.funcPC)
			if funcInfo.valid() {
				file, line := funcline(funcInfo, ctx.createPC)
				print(funcname(funcInfo), " (", kind, ")\n")
				print("\t", file, ":", line, "\n")
			} else {
				print("<bad pc ", hex(ctx.funcPC), ">\n")
			}

			print("created at: ")
			createInfo := findfunc(ctx.createPC)
			if createInfo.valid() {
				file, line := funcline(createInfo, ctx.createPC)
				print(funcname(createInfo), "\n")
				print("\t", file, ":", line, "\n")
			} else {
				print("<bad pc ", hex(ctx.createPC), ">\n")
			}

			lastPtr = r.ptr
		}
		println()
		if more {
			println("runtime: too many errors")
		}
		throw("runtime: detected possible cleanup and/or finalizer leaks")
	}
}

// forEachSpecial is an iterator over all specials.
//
// Used by debug.checkfinalizers.
//
// The world must be stopped.
func forEachSpecial(yield func(p uintptr, s *mspan, sp *special) bool) {
	assertWorldStopped()

	// Find the arena and page index into that arena for this shard.
	for _, ai := range mheap_.markArenas {
		ha := mheap_.arenas[ai.l1()][ai.l2()]

		// Construct slice of bitmap which we'll iterate over.
		for i := range ha.pageSpecials[:] {
			// Find set bits, which correspond to spans with specials.
			specials := atomic.Load8(&ha.pageSpecials[i])
			if specials == 0 {
				continue
			}
			for j := uint(0); j < 8; j++ {
				if specials&(1<<j) == 0 {
					continue
				}
				// Find the span for this bit.
				//
				// This value is guaranteed to be non-nil because having
				// specials implies that the span is in-use, and since we're
				// currently marking we can be sure that we don't have to worry
				// about the span being freed and re-used.
				s := ha.spans[uint(i)*8+j]

				// Lock the specials to prevent a special from being
				// removed from the list while we're traversing it.
				for sp := s.specials; sp != nil; sp = sp.next {
					if !yield(s.base()+sp.offset, s, sp) {
						return
					}
				}
			}
		}
	}
}
