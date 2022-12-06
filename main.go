// This is a MODIFIED Go #2 binary tree from the Benchmarks Game:
//   https://benchmarksgame-team.pages.debian.net/benchmarksgame/program/binarytrees-go-2.html
//
// Modifications include:
//  * adding arenas support
//  * -minalloc flag controls how frequently each worker goroutine calls Free
//  * -single flag creates 1 tree in 1 goroutine
//  * -cpuprofile and -memprofile flags for pprof
//  * default to binary tree depth of 21 if not specified via command line
//  * slightly modified output
//
// License is 3-Clause BSD:
//   https://benchmarksgame-team.pages.debian.net/benchmarksgame/license.html
//
// Leaving the following comments as-is from the original Go #2 program:
// -------------------------------------------------------
//
// The Computer Language Benchmarks Game
// http://benchmarksgame.alioth.debian.org/
//
// Go implementation of binary-trees, based on the reference implementation
// gcc #3, on Go #8 (which is based on Rust #4)
//
// Comments aim to be analogous as those in the reference implementation and are
// intentionally verbose, to help programmers unexperienced in GO to understand
// the implementation.
//
// The following alternative implementations were considered before submitting
// this code. All of them had worse readability and didn't yield better results
// on my local machine:
//
// 0. general:
// 0.1 using uint32, instead of int;
//
// 1. func Count:
// 1.1 using a local stack, instead of using a recursive implementation; the
//     performance degraded, even using a pre-allocated slice as stack and
//     manually handling its size;
// 1.2 assigning Left and Right to nil after counting nodes; the idea to remove
//     references to instances no longer needed was to make GC easier, but this
//     did not work as intended;
// 1.3 using a walker and channel, sending 1 on each node; although this looked
//     idiomatic to GO, the performance suffered a lot;
// 2. func NewTree:
// 2.1 allocating all tree nodes on a tree slice upfront and making references
//     to those instances, instead of allocating two sub-trees on each call;
//     this did not improve performance;
//
// Contributed by Gerardo Lima
// Reviewed by Diogo Simoes
// Based on previous work from Adam Shaver, Isaac Gouy, Marcel Ibes Jeremy,
//  Zerfas, Jon Harrop, Alex Mizrahi, Bruno Coutinho, ...
//

package main

import (
	"arena"
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
)

// minalloc flag controls how frequently each worker goroutine calls Free
var minAllocMB = flag.Float64("minalloc", 1, "upon completing a tree, a worker goroutine "+
	"reuses its arena unless the arena has completed more than minalloc `MB` of allocations")
var single = flag.Bool("single", false, "allocate one tree in a single goroutine")

var (
	cpuprofile = flag.String("cpuprofile", "", "write cpu profile to `file`")
	memprofile = flag.String("memprofile", "", "write memory profile to `file`")
)

type Tree struct {
	Left  *Tree
	Right *Tree
}

// Count the nodes in the given complete binary tree.
func (t *Tree) Count() int {
	// Only test the Left node (this binary tree is expected to be complete).
	if t.Left == nil {
		return 1
	}
	return 1 + t.Right.Count() + t.Left.Count()
}

// Create a complete binary tree of `depth` and return it as a pointer.
func NewTree(depth int, a *arena.Arena) *Tree {
	// thepudds: alloc via an arena if we have one.
	if depth > 0 {
		// thepudds: note that for this particular benchmark, it is faster to create the
		// left and right sub-trees before allocating our own tree node.
		// Otherwise, we could eliminate a couple of lines here.
		left := NewTree(depth-1, a)
		right := NewTree(depth-1, a)
		treePtr := allocTreeNode(a)
		treePtr.Left = left
		treePtr.Right = right
		return treePtr
	} else {
		return allocTreeNode(a)
	}
}

// Allocate an empty tree node, using an arena if provided.
func allocTreeNode(a *arena.Arena) *Tree {
	if a != nil {
		return arena.New[Tree](a)
	} else {
		return &Tree{}
	}
}

func Run(maxDepth int) {
	var wg sync.WaitGroup

	// Set minDepth to 4 and maxDepth to the maximum of maxDepth and minDepth +2.
	const minDepth = 4
	if maxDepth < minDepth+2 {
		maxDepth = minDepth + 2
	}

	// Create an indexed string buffer for outputing the result in order.
	outCurr := 0
	outSize := 3 + (maxDepth-minDepth)/2
	outBuff := make([]string, outSize)

	// Create binary tree of depth maxDepth+1, compute its Count and set the
	// first position of the outputBuffer with its statistics.
	wg.Add(1)
	go func() {
		// thepudds: create a single arena for this single (usually large) tree,
		// freeing it when we are done with this tree.
		stretchArena := arena.NewArena()
		defer stretchArena.Free()

		tree := NewTree(maxDepth+1, stretchArena)
		nodes := tree.Count()
		msg := fmt.Sprintf("   stretch tree of depth %-8d arenas: %-6d nodes: %-10d MB: %0.1f",
			maxDepth+1,
			1,
			nodes,
			float64(nodes*16)/(1<<20))

		outBuff[0] = msg
		wg.Done()
	}()
	if *single {
		// thepudds: only do a single tree (with only one goroutine)
		wg.Wait()
		return
	}

	// Create a long-lived binary tree of depth maxDepth. Its statistics will be
	// handled later.
	var longLivedTree *Tree
	wg.Add(1)
	// thepudds: also create a long-lived arena for this long-lived tree,
	// freeing it when we are done with this function.
	longLivedArena := arena.NewArena()
	defer longLivedArena.Free()

	go func() {
		longLivedTree = NewTree(maxDepth, longLivedArena)
		wg.Done()
	}()

	// Create a lot of binary trees, of depths ranging from minDepth to maxDepth,
	// compute and tally up all their Count and record the statistics.
	for depth := minDepth; depth <= maxDepth; depth += 2 {
		iterations := 1 << (maxDepth - depth + minDepth)
		outCurr++

		wg.Add(1)
		go func(depth, iterations, index int) {
			// Create a binary tree of depth and accumulate total counter with its
			// node count.

			// thepudds: Also create an arena for the binary tree allocations for this goroutine.
			// We reuse each arena until it has allocated more than minAllocMB.
			treeArena := arena.NewArena()
			arenaCount := 1
			allocated := 0

			nodes := 0
			for i := 0; i < iterations; i++ {
				if allocated > int(*minAllocMB*(1<<20)) {
					treeArena.Free()
					treeArena = arena.NewArena()
					arenaCount++
					allocated = 0
				}
				tree := NewTree(depth, treeArena)
				newNodes := tree.Count()
				nodes += newNodes
				allocated += newNodes * 16
			}

			msg := fmt.Sprintf(" %8d trees of depth %-8d arenas: %-6d nodes: %-10d MB: %0.1f",
				iterations,
				depth,
				arenaCount,
				nodes,
				float64(nodes*16)/(1<<20))
			outBuff[index] = msg

			treeArena.Free()
			wg.Done()
		}(depth, iterations, outCurr)
	}

	wg.Wait()

	// Compute the checksum of the long-lived binary tree that we created
	// earlier and store its statistics.
	nodes := longLivedTree.Count()
	msg := fmt.Sprintf("long lived tree of depth %-8d arenas: %-6d nodes: %-10d MB: %0.1f",
		maxDepth,
		1,
		nodes,
		float64(nodes*16)/(1<<20))
	outBuff[outSize-1] = msg

	// Print the statistics for all of the various tree depths.
	for _, m := range outBuff {
		fmt.Println(m)
	}
}

func main() {
	flag.Parse()

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal("could not create CPU profile: ", err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal("could not start CPU profile: ", err)
		}
		defer pprof.StopCPUProfile()
	}

	if *memprofile != "" {
		defer func() {
			f, err := os.Create(*memprofile)
			if err != nil {
				log.Fatal("could not create memory profile: ", err)
			}
			defer f.Close()
			runtime.GC() // get up-to-date statistics
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Fatal("could not write memory profile: ", err)
			}
		}()
	}

	n := 21
	if flag.NArg() > 0 {
		var err error
		n, err = strconv.Atoi(flag.Arg(0))
		if err != nil {
			log.Fatal("must specify binary tree depth as integer: ", err)
		}
	}

	Run(n)
}
