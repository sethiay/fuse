// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package forgetfs

import (
	"fmt"
	"os"
	"runtime"

	"github.com/jacobsa/fuse"
	"github.com/jacobsa/fuse/fuseops"
	"github.com/jacobsa/fuse/fuseutil"
	"github.com/jacobsa/gcloud/syncutil"
)

// Create a file system whose sole contents are a file named "foo" and a
// directory named "bar".
//
// The file "foo" may be opened for reading and/or writing, but reads and
// writes aren't supported. Additionally, any non-existent file or directory
// name may be created within any directory, but the resulting inode will
// appear to have been unlinked immediately.
//
// The file system maintains reference counts for the inodes involved. It will
// panic if a reference count becomes negative or if an inode ID is re-used
// after we expect it to be dead. Its Check method may be used to check that
// there are no inodes with unexpected reference counts remaining, after
// unmounting.
func NewFileSystem() (fs *ForgetFS) {
	// Set up the actual file system.
	impl := &fsImpl{
		inodes: map[fuseops.InodeID]*inode{
			cannedID_Root: &inode{
				attributes: fuseops.InodeAttributes{
					Nlink: 1,
					Mode:  0777 | os.ModeDir,
				},
			},
			cannedID_Foo: &inode{
				attributes: fuseops.InodeAttributes{
					Nlink: 1,
					Mode:  0777,
				},
			},
			cannedID_Bar: &inode{
				attributes: fuseops.InodeAttributes{
					Nlink: 1,
					Mode:  0777 | os.ModeDir,
				},
			},
		},
		nextInodeID: cannedID_Next,
	}

	// The root inode starts with a lookup count of one.
	impl.inodes[cannedID_Root].IncrementLookupCount()

	// Set up the mutex.
	impl.mu = syncutil.NewInvariantMutex(impl.checkInvariants)

	// Set up a wrapper that exposes only certain methods.
	fs = &ForgetFS{
		impl:   impl,
		server: fuseutil.NewFileSystemServer(impl),
	}

	return
}

////////////////////////////////////////////////////////////////////////
// ForgetFS
////////////////////////////////////////////////////////////////////////

type ForgetFS struct {
	impl   *fsImpl
	server fuse.Server
}

func (fs *ForgetFS) ServeOps(c *fuse.Connection) {
	fs.server.ServeOps(c)
}

// Panic if there are any inodes that have a non-zero reference count. For use
// after unmounting.
func (fs *ForgetFS) Check() {
	fs.impl.Check()
}

////////////////////////////////////////////////////////////////////////
// Actual implementation
////////////////////////////////////////////////////////////////////////

const (
	cannedID_Root = fuseops.RootInodeID + iota
	cannedID_Foo
	cannedID_Bar
	cannedID_Next
)

type fsImpl struct {
	fuseutil.NotImplementedFileSystem

	/////////////////////////
	// Mutable state
	/////////////////////////

	mu syncutil.InvariantMutex

	// An index of inode by ID, for all IDs we have issued.
	//
	// GUARDED_BY(mu)
	inodes map[fuseops.InodeID]*inode

	// The next ID to issue.
	//
	// INVARIANT: For each k in inodes, k < nextInodeID
	//
	// GUARDED_BY(mu)
	nextInodeID fuseops.InodeID
}

////////////////////////////////////////////////////////////////////////
// inode
////////////////////////////////////////////////////////////////////////

type inode struct {
	attributes fuseops.InodeAttributes

	// The current lookup count.
	lookupCount uint64

	// true if lookupCount has ever been positive.
	lookedUp bool
}

func (in *inode) Forgotten() bool {
	return in.lookedUp && in.lookupCount == 0
}

func (in *inode) IncrementLookupCount() {
	in.lookupCount++
	in.lookedUp = true
}

func (in *inode) DecrementLookupCount(n uint64) {
	if in.lookupCount < n {
		panic(fmt.Sprintf(
			"Overly large decrement: %v, %v",
			in.lookupCount,
			n))
	}

	in.lookupCount -= n
}

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

// LOCKS_REQUIRED(fs.mu)
func (fs *fsImpl) checkInvariants() {
	// INVARIANT: For each k in inodes, k < nextInodeID
	for k, _ := range fs.inodes {
		if !(k < fs.nextInodeID) {
			panic("Unexpectedly large inode ID")
		}
	}
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *fsImpl) Check() {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// On Linux we often don't receive forget ops, and never receive destroy ops
	// (cf. http://goo.gl/EUbxEg, fuse-devel thread "Root inode lookup count").
	// So there's not really much we can check here.
	//
	// TODO(jacobsa): Figure out why we don't receive destroy. If we can reliably
	// receive it, we can treat it as "forget all".
	if runtime.GOOS == "linux" {
		return
	}

	for k, v := range fs.inodes {
		// Special case: we don't require the root inode to have reached zero.
		// OS X doesn't seem to send forgets for the root, and Linux only does
		// sometimes. But we want to make sure it's not greater than one, which
		// would be weird.
		if k == fuseops.RootInodeID {
			if v.lookupCount > 1 {
				panic(fmt.Sprintf("Root has lookup count %v", v.lookupCount))
			}

			continue
		}

		// Check other inodes.
		if v.lookupCount != 0 {
			panic(fmt.Sprintf("Inode %v has lookup count %v", k, v.lookupCount))
		}
	}
}

// Look up the inode and verify it hasn't been forgotten.
//
// LOCKS_REQUIRED(fs.mu)
func (fs *fsImpl) findInodeByID(id fuseops.InodeID) (in *inode) {
	in = fs.inodes[id]
	if in == nil {
		panic(fmt.Sprintf("Unknown inode: %v", id))
	}

	if in.Forgotten() {
		panic(fmt.Sprintf("Forgotten inode: %v", id))
	}

	return
}

////////////////////////////////////////////////////////////////////////
// FileSystem methods
////////////////////////////////////////////////////////////////////////

func (fs *fsImpl) Init(
	op *fuseops.InitOp) (err error) {
	return
}

func (fs *fsImpl) LookUpInode(
	op *fuseops.LookUpInodeOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Make sure the parent exists and has not been forgotten.
	_ = fs.findInodeByID(op.Parent)

	// Handle the names we support.
	var childID fuseops.InodeID
	switch {
	case op.Parent == cannedID_Root && op.Name == "foo":
		childID = cannedID_Foo

	case op.Parent == cannedID_Root && op.Name == "bar":
		childID = cannedID_Bar

	default:
		err = fuse.ENOENT
		return
	}

	// Look up the child.
	child := fs.findInodeByID(childID)
	child.IncrementLookupCount()

	// Return an appropriate entry.
	op.Entry = fuseops.ChildInodeEntry{
		Child:      childID,
		Attributes: child.attributes,
	}

	return
}

func (fs *fsImpl) GetInodeAttributes(
	op *fuseops.GetInodeAttributesOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Find the inode, verifying that it has not been forgotten.
	in := fs.findInodeByID(op.Inode)

	// Return appropriate attributes.
	op.Attributes = in.attributes

	return
}

func (fs *fsImpl) ForgetInode(
	op *fuseops.ForgetInodeOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Find the inode and decrement its count.
	in := fs.findInodeByID(op.Inode)
	in.DecrementLookupCount(op.N)

	return
}

func (fs *fsImpl) MkDir(
	op *fuseops.MkDirOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Make sure the parent exists and has not been forgotten.
	_ = fs.findInodeByID(op.Parent)

	// Mint a child inode.
	childID := fs.nextInodeID
	fs.nextInodeID++

	child := &inode{
		attributes: fuseops.InodeAttributes{
			Nlink: 0,
			Mode:  0777 | os.ModeDir,
		},
	}

	fs.inodes[childID] = child
	child.IncrementLookupCount()

	// Return an appropriate entry.
	op.Entry = fuseops.ChildInodeEntry{
		Child:      childID,
		Attributes: child.attributes,
	}

	return
}

func (fs *fsImpl) CreateFile(
	op *fuseops.CreateFileOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Make sure the parent exists and has not been forgotten.
	_ = fs.findInodeByID(op.Parent)

	// Mint a child inode.
	childID := fs.nextInodeID
	fs.nextInodeID++

	child := &inode{
		attributes: fuseops.InodeAttributes{
			Nlink: 0,
			Mode:  0777,
		},
	}

	fs.inodes[childID] = child
	child.IncrementLookupCount()

	// Return an appropriate entry.
	op.Entry = fuseops.ChildInodeEntry{
		Child:      childID,
		Attributes: child.attributes,
	}

	return
}

func (fs *fsImpl) OpenFile(
	op *fuseops.OpenFileOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Verify that the inode has not been forgotten.
	_ = fs.findInodeByID(op.Inode)

	return
}

func (fs *fsImpl) OpenDir(
	op *fuseops.OpenDirOp) (err error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Verify that the inode has not been forgotten.
	_ = fs.findInodeByID(op.Inode)

	return
}